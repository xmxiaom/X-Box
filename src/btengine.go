package main

// 内置 BT 引擎 · 任务层：磁力入队 →（tracker+DHT）取元数据 → 分片下载 → 落盘 → 完成做种。
//
// 编排模型（每个种子一个 goroutine 群，由 btCreate 启动 runTask）：
//   runTask(t)
//     ├─ peer 喂料：tracker announce 轮询 + DHT findPeers 并行塞 peerCh
//     ├─ 阶段1 fetching：向批量 peer 抓 ut_metadata（BEP9），先到先得并核对 infohash
//     └─ 阶段2 downloading：worker 池各持一条连接按顺序抢片要块（顺序选片，进度单调），
//         每片 SHA1 校验通过才落盘
//
// 状态机：pending → fetching → downloading → done / error / stopped
//
// 安全红线：
//   - 任务目录必须过 safeDownloadDir + MkdirAll；
//   - 种子总大小上限 4GiB；piece 校验通过才写正盘；文件名 btClean* 清洗防逃逸；
//   - worker panic 只标记任务（recover），绝不炸进程。

import (
	"context"
	crand "crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	btMaxTasks       = 8               // 同时任务数上限（设置页可在 1-8 调）
	btMetaTryTimeout = 3 * time.Minute // 元数据阶段总窗口（DHT 冷启动/冷门做种需要余地）
	btMetaMaxPeers   = 64              // 元数据阶段并发拨号上限（先到先得；调大加速出种。v1.3.49 从 24 提高——48+ peer 的热门种 24 路不够吃，拨号失败快速回收后空槽闲置）
	btMetaIPCap      = 3               // 同一 IP 最多尝试的端口数（防假 seeder 农场占满并发槽）
	btMaxTrackers    = 40              // 合并磁力自带(含 udp://) + 公共 tracker 的上限（v1.3.53 从 20 提高，让公共 tracker 全量进列表）
	btMaxTorrentSz   = int64(4) << 30
	btBlockLen       = 16 << 10 // 分块 16KB（协议标准值，不能改）
	btStalePiece     = 40 * time.Second
	btPeersChCap     = 256
	// 未放行(choke)等待窗口：做种者 optimistic-unchoke 约 30s 轮换一次、通常只放行少数
	// 连接。被 choke 的连接需熬过 1~2 轮放行才有机会被真正青睐；设 2 分钟，过后仍没
	// unchoke 才让出并发槽去拨别的 peer。太短会在轮换还没轮到自己时就断开，导致热议种
	// 的连接数长期只剩被乐观放行的 1~2 个（v1.3.53 提速后仍上不去的根因之一）。
	btUnchokeWait = 2 * time.Minute
	btSpeedWin    = 10 // 速度滑动平均窗口（秒）：窗口越长越平滑，整片写盘的空档不会让速度掉到 0
	// 端游(Endgame)阈值：剩余未完成片 ≤ 该值时进入竞争收尾模式。
	// 下载快结束时只有少数慢片，独占 worker 若被慢 peer 拖着，其余空闲 worker
	// 空等退场 → 「最后一点点越下越慢」。端游让空闲 worker 竞速这些尾片，
	// 谁先拼齐谁写盘（写盘幂等，快 peer 不必再被慢 peer 拖死）。
	btEndgamePieces = 2

	// 并发默认值（设置页可调；见 dlDefMaxConns / dlDefPipeline）
	// 迅雷/qBittorrent 式提速：把默认并发与管线放宽，让同一任务在更多 peer 上并行取块、
	// 单连接窗口更饱满，高 RTT 或慢速 peer 不再成为整速天花板。
	btDefMaxConns = 24 // 单任务并发连接（v1.3.53 从 16 提高——16 路对热门种常吃不饱带宽）
	btDefPipeline = 24 // 单连接挂起块数（24*16KB = 384KB 管线）
	btMaxConnsCap = 64 // 单任务并发连接硬上限（v1.3.53 从 40 提高）
	btPipeCap     = 48 // 单连接挂起块数硬上限（v1.3.53 从 32 提高）
)

// 运行时并发参数（由 dlConfig 注入；未设置时用默认值）
var btTune struct {
	mu       sync.RWMutex
	maxConns int
	pipeline int
}

func btTuneInit() {
	btTune.mu.Lock()
	if btTune.maxConns == 0 {
		btTune.maxConns = btDefMaxConns
	}
	if btTune.pipeline == 0 {
		btTune.pipeline = btDefPipeline
	}
	btTune.mu.Unlock()
}

// btApplyTune 应用配置（任务启动/设置变更时调用）
func btApplyTune(cfg DlConfig) {
	btTune.mu.Lock()
	btTune.maxConns = dlClampInt(cfg.MaxConns, 4, btMaxConnsCap, btDefMaxConns)
	btTune.pipeline = dlClampInt(cfg.Pipeline, 4, btPipeCap, btDefPipeline)
	btTune.mu.Unlock()
	btStore.lim.setLimit(dlClampInt(cfg.MaxTasks, 1, btMaxTasks, dlDefMaxTasks))
}

func btTuneConns() int {
	btTuneInit()
	btTune.mu.RLock()
	defer btTune.mu.RUnlock()
	return btTune.maxConns
}

func btTunePipeline() int {
	btTuneInit()
	btTune.mu.RLock()
	defer btTune.mu.RUnlock()
	return btTune.pipeline
}

// btLimiter 可调并发限流器（同时任务数可运行时调整）
type btLimiter struct {
	mu      sync.Mutex
	cond    *sync.Cond
	limit   int
	running int
}

func (l *btLimiter) init(limit int) {
	l.mu.Lock()
	if l.cond == nil {
		l.cond = sync.NewCond(&l.mu)
	}
	if l.limit <= 0 {
		l.limit = limit
	}
	l.mu.Unlock()
}

// setLimit 调整上限（调大立刻唤醒等待者）
func (l *btLimiter) setLimit(n int) {
	l.mu.Lock()
	l.limit = n
	l.mu.Unlock()
	l.cond.Broadcast()
}

// acquire 占一个槽（阻塞直到有空位或 ctx 取消）。返回 false 表示 ctx 已取消。
func (l *btLimiter) acquire(ctx context.Context) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for l.running >= l.limit {
		if ctx.Err() != nil {
			return false
		}
		// 用 goroutine 把 ctx 取消转成 Broadcast，避免 Cond 无法感知 ctx
		stop := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				l.cond.Broadcast()
			case <-stop:
			}
		}()
		l.cond.Wait()
		close(stop)
	}
	if ctx.Err() != nil {
		return false
	}
	l.running++
	return true
}

// release 释放槽
func (l *btLimiter) release() {
	l.mu.Lock()
	if l.running > 0 {
		l.running--
	}
	l.mu.Unlock()
	l.cond.Broadcast()
}

// btDefaultTrackers 公共 tracker 兜底：磁力自带 tr 列表薄时保证 peer 发现半径。
// 只用 http/https + udp（btTrackerLoop 走 http/https，btUDPTrackerLoop 走 BEP15 udp）。
var btDefaultTrackers = []string{
	// 一线公共 tracker（全球用户量最大的几个，务必排前，命中率高）
	"udp://tracker.opentrackr.org:1337/announce",
	"http://tracker.opentrackr.org:1337/announce",
	"udp://open.tracker.cl:1337/announce",
	"http://open.tracker.cl:1337/announce",
	"udp://exodus.desync.com:6969/announce",
	"http://exodus.desync.com:6969/announce",
	"http://tracker.openbittorrent.com:6969/announce",
	"udp://tracker.openbittorrent.com:6969/announce",
	"https://tracker.tamersunion.org:443/announce",
	"udp://tracker.torrent.eu.org:451/announce",
	"https://tracker.gbitt.info:443/announce",
	"udp://tracker.altrosky.nl:6969/announce",
	"udp://open.demonii.com:1337/announce",
	"http://tracker.files.fm:6969/announce",
	"http://tracker.bt4g.com:2095/announce",
	// 国内可直连、对中文资源 peer 覆盖面较好的 tracker
	"http://t.itzmx.com:8080/announce",
	"udp://t.itzmx.com:8080/announce",
}

// btMergeTrackers 磁力自带 tr 优先，公共 tracker 兜底，去重截断。
// udp:// 全部接纳（BEP15 已实现）；不带端口的 udp:// host 补 ：80。
func btMergeTrackers(tr []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range append(append([]string{}, tr...), btDefaultTrackers...) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		if strings.HasPrefix(s, "udp://") {
			if u, err := url.Parse(s); err == nil && u.Port() == "" && u.Hostname() != "" {
				s = "udp://" + u.Hostname() + ":80" + u.Path
			}
		}
		seen[s] = true
		out = append(out, s)
		if len(out) >= btMaxTrackers {
			break
		}
	}
	return out
}

// btTask 单个种子任务
type btTask struct {
	mu        sync.Mutex
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Dir       string    `json:"dir"`
	Status    string    `json:"status"` // pending/fetching/downloading/done/error/stopped
	Err       string    `json:"err,omitempty"`
	CreatedAt time.Time `json:"createdAt"`

	hash    [20]byte
	meta    *btMeta
	have    []bool
	doneN   int
	written int64

	speed int64

	peersActive int
	peersReady  int                  // 握手完成的可用连接数（展示用；peersActive 含拨号尝试，脉冲式涨跌会误导）
	peerSeen    map[string]time.Time // 见过的候选地址 → 最近一次派工时间（持续补拨用，v1.3.49）
	seenN       int                  // 已发现的候选 peer 数（fetching 态诊断展示）
	inflight    map[string]time.Time // 最近连过的 peer（30s 内不重复拨）

	cancel context.CancelFunc
	ctx    context.Context

	// 暂停/续传（v1.3.43）：paused 期间保留 meta/have/prep（已落盘分片 + 文件句柄），
	// 恢复时从断点继续，不重建文件、不重抓元数据。
	// 暂停即退出工作轮次（释放并发槽，不占 btMaxTasks），resume 重新拉起；
	// meta/have/prep 全保留 → 真断点续传（不重抓元数据、不重开文件、不重下已完成分片）。
	paused     bool
	pauseGen   int                // 暂停代次：每暂停一次 +1；轮次据此判定「被暂停打断」而非「停止」
	active     bool               // 是否有工作轮次在跑（占着并发槽）；暂停后为 false
	workCancel context.CancelFunc // 当前工作轮次取消（暂停/停止都打它）
	prep       *btPrepared        // 文件布局（续传复用，避免 uniquePath 改名/截断）
	trackers   []string           // 合并后的 tracker 列表（续传重新拉起时复用）

	reuseFiles bool // 断点恢复（v1.3.52）：预创建文件时沿用原文件名，不再造「xxx (1)」副本

	testPeers []string // 测试注入：只直连这些地址（跳过 tracker/DHT）
}

var btStore struct {
	mu    sync.Mutex
	tasks map[string]*btTask
	order []string
	lim   btLimiter // 同时任务数（可运行时调整）
	once  sync.Once
}

func btInit() {
	btStore.once.Do(func() {
		btStore.tasks = map[string]*btTask{}
		btStore.lim.init(dlDefMaxTasks)
		dhtInit()
	})
}

// btCreate 入队（同种子重复点击返回已有任务）
func btCreate(magnet, dir, title string) (*btTask, error) {
	return btCreateEx(magnet, dir, title, nil)
}

// btCreateEx 变体：testPeers 非 nil 时只直连这些地址（单测 fake seeder 用）
func btCreateEx(magnet, dir, title string, testPeers []string) (*btTask, error) {
	t, _, err := btCreateDedup(magnet, dir, title, testPeers)
	return t, err
}

// btCreateDedup 入队并返回是否命中已有任务（dup=true 表示没有新建，直接复用了同种子的既有任务）。
// 去重口径：同一 infohash 只要处于 pending/fetching/downloading/paused/done 任一种状态就复用，
// 只有 error/stopped（明确失败或被停止）才允许重新创建 —— 否则会重复下载同一资源。
func btCreateDedup(magnet, dir, title string, testPeers []string) (t *btTask, dup bool, err error) {
	btInit()
	btApplyTune(loadDlConfig()) // 设置页的并发/线程配置在每次入队时生效
	hash, dn, trackers, err := btMagnet(magnet)
	if err != nil {
		return nil, false, err
	}
	if _, safe := safeDownloadDir(dir); !safe {
		return nil, false, fmt.Errorf("下载目录不可用：%s", dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, false, fmt.Errorf("创建目录失败：%s", err.Error())
	}
	// 同种子（同 infohash）已在队列：直接返回既有任务，不重复入队/重复下载
	btStore.mu.Lock()
	for _, ex := range btStore.tasks {
		if ex.hash == hash {
			ex.mu.Lock()
			st := ex.Status
			ex.mu.Unlock()
			// 只有明确失败/被停止的旧任务才允许重建（用户可能想重试）
			if st != "error" && st != "stopped" {
				btStore.mu.Unlock()
				return ex, true, nil
			}
		}
	}
	btStore.mu.Unlock()
	if title == "" {
		title = dn
	}
	if title == "" {
		title = hashHexShort(hash)
	}
	id := btNewTaskID(hash)
	t = &btTask{
		ID: id, Title: title, Dir: dir, Status: "pending",
		CreatedAt: time.Now(), inflight: map[string]time.Time{},
		hash:      hash,
		testPeers: testPeers,
	}
	t.ctx, t.cancel = context.WithCancel(context.Background())
	btStore.mu.Lock()
	btStore.tasks[id] = t
	btStore.order = append(btStore.order, id)
	btStore.mu.Unlock()

	// 队列满时不阻塞 API：后台等槽位（30 分钟排队上限），拿到槽才开始跑
	t.mu.Lock()
	t.trackers = trackers
	t.mu.Unlock()
	go func() {
		qctx, qcancel := context.WithTimeout(t.ctx, 30*time.Minute)
		defer qcancel()
		if !btStore.lim.acquire(qctx) {
			if qctx.Err() != nil && t.ctx.Err() == nil {
				t.setLock("error", "排队超时（同时任务过多）")
			}
			return
		}
		defer btStore.lim.release()
		runTask(t, trackers)
	}()
	return t, false, nil
}

func hashHexShort(h [20]byte) string { return btHashHex(h)[:8] }

// btTaskIDSeq 任务 id 自增序号：UnixNano%1e6 在极短间隔内会撞号（同一微秒内多次提交），
// 叠一个进程内自增序号保证唯一（同种子停止后重建也不会拿到旧 id）。
var btTaskIDSeq uint64

func btNewTaskID(hash [20]byte) string {
	n := atomic.AddUint64(&btTaskIDSeq, 1)
	return "bt" + btHashHex(hash)[:12] + strconv.FormatInt(time.Now().UnixNano()%1e6, 10) + "-" + strconv.FormatUint(n, 10)
}

// setLock 锁内置终态
func (t *btTask) setLock(status, errMsg string) {
	t.mu.Lock()
	t.Status = status
	t.Err = errMsg
	t.mu.Unlock()
}

// runTask 单轮任务执行（占一个并发槽）：抓元数据 → 下载。暂停/停止时返回，
// 由调度层（btStartWork）决定是否在恢复后重新拉起新的一轮。
// 退出时不释放 prep（暂停要续传），只有终态才在 btReleasePrep 里释放。
func runTask(t *btTask, trackers []string) {
	defer func() {
		if r := recover(); r != nil {
			t.setLock("error", "内部错误："+fmt.Sprint(r))
		}
	}()
	// 首轮把合并后的 tracker 存进任务，续传时直接用（不重复 merge）
	t.mu.Lock()
	if t.trackers == nil {
		t.trackers = trackers
	}
	wctx, wcancel := context.WithCancel(t.ctx)
	t.workCancel = wcancel
	t.active = true
	genAtStart := t.pauseGen // 记录本轮启动时的暂停代次（区分「暂停打断」与「停止」）
	t.mu.Unlock()

	reason := btRunOnce(t, trackers, wctx, genAtStart)
	wcancel()

	t.mu.Lock()
	t.workCancel = nil
	t.active = false
	t.mu.Unlock()

	if reason == "paused" || reason == "stopped" {
		// 暂停后又立刻点继续：resume 时本轮可能还没退干净（active 仍 true，
		// btStartWork 被挡），在此接管重启，避免任务停在运行态却没人干活。
		if reason == "paused" && !t.btIsPaused() {
			btStartWork(t)
		}
		return
	}
	btReleasePrep(t) // done/error 才真正放掉文件句柄
}

// btReleasePrep 释放文件布局（终态收尾）
func btReleasePrep(t *btTask) {
	t.mu.Lock()
	p := t.prep
	t.prep = nil
	t.mu.Unlock()
	if p != nil {
		p.btCloseAllPrep()
	}
}

// btStartWork 拉起一轮工作（占并发槽）；已在跑或已终态则忽略。
func btStartWork(t *btTask) {
	btInit()
	t.mu.Lock()
	if t.active || t.ctx.Err() != nil {
		t.mu.Unlock()
		return
	}
	st := t.Status
	if !btActiveStatus(st) && st != "paused" {
		t.mu.Unlock()
		return
	}
	trs := t.trackers
	t.mu.Unlock()

	go func() {
		if !btStore.lim.acquire(t.ctx) {
			return
		}
		defer btStore.lim.release()
		runTask(t, trs)
	}()
}

// btIsPaused 当前是否处于用户暂停态
func (t *btTask) btIsPaused() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.paused
}

// btStopReason 工作轮次被 ctx 打断时的归因。
// 关键：暂停后立刻点继续时 paused 已被清 false，若只看 paused 会把「暂停打断」
// 误判成「停止」→ 任务直接进 stopped 终态。故用轮次启动时记录的暂停代次比对：
// 代次变了说明本轮是被暂停打断的，应保持 paused 状态等下一轮。
func (t *btTask) btStopReason(genAtStart int) string {
	t.mu.Lock()
	changed := t.pauseGen != genAtStart
	paused := t.paused
	t.mu.Unlock()
	if changed || paused {
		return "paused"
	}
	t.setLock("stopped", "")
	return "stopped"
}

// btRunOnce 单轮工作：抓元数据（已有则跳过）→ 文件布局（已有则复用）→ worker 池。
// 返回 "done" / "error" / "stopped" / "paused"。
func btRunOnce(t *btTask, trackers []string, ctx context.Context, genAtStart int) string {
	defer func() {
		if r := recover(); r != nil {
			t.setLock("error", "内部错误："+fmt.Sprint(r))
		}
	}()

	// 首次进入才置 fetching；续传（meta 已在）直接回到下载态
	t.mu.Lock()
	hasMeta := t.meta != nil
	t.mu.Unlock()
	if !hasMeta {
		t.setLock("fetching", "")
	}

	// 速度打点：每秒采样 written 增量，显示值取最近 10 秒平均。
	// 整片校验通过才写盘 → 瞬时差会时不时为 0（大分片尤其明显），
	// 窗口太短前端就会「速度一会有一会没有」，故拉长滑动窗口做平滑。
	go func() {
		defer func() { recover() }()
		tk := time.NewTicker(1 * time.Second)
		defer tk.Stop()
		t.mu.Lock()
		prev := t.written
		t.mu.Unlock()
		samples := make([]int64, 0, btSpeedWin)
		for {
			select {
			case <-ctx.Done():
				t.mu.Lock()
				t.speed = 0
				t.mu.Unlock()
				return
			case <-tk.C:
				t.mu.Lock()
				cur := t.written
				delta := cur - prev
				prev = cur
				t.mu.Unlock()
				if delta < 0 {
					delta = 0
				}
				samples = append(samples, delta)
				if len(samples) > btSpeedWin {
					samples = samples[1:]
				}
				var sum int64
				for _, s := range samples {
					sum += s
				}
				avg := sum / int64(len(samples))
				t.mu.Lock()
				// 窗口内全为 0 但仍有活跃连接：说明只是「整片在攒、还没校验写盘」的空档，
				// 不是真的停摆。此时保留上次非零值，避免速度数字掉成 0 又跳回去地闪。
				if avg == 0 && t.peersActive > 0 && t.speed > 0 {
					// 缓慢衰减（每轮 -12.5%），长时间无写盘仍会自然降到 0
					t.speed = t.speed - t.speed/8
				} else {
					t.speed = avg
				}
				t.mu.Unlock()
			}
		}
	}()

	peerCh := make(chan btPeerAddr, btPeersChCap)
	feed := func(a btPeerAddr) {
		select {
		case peerCh <- a:
		default:
		}
	}
	if t.testPeers != nil {
		// 持续喂注入 peer（模拟 tracker/DHT 流），阶段2 拉起 worker 也靠它
		go func() {
			defer func() { recover() }()
			for {
				if ctx.Err() != nil {
					return
				}
				for _, s := range t.testPeers {
					host, port, cerr := net.SplitHostPort(s)
					if cerr != nil || port == "" {
						continue
					}
					if pn, aerr := strconv.Atoi(port); aerr == nil && pn > 0 {
						feed(btPeerAddr{Host: host, Port: pn})
					}
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(2 * time.Second):
				}
			}
		}()
	} else {
		trackers = btMergeTrackers(trackers)
		for _, tr := range trackers {
			if strings.HasPrefix(tr, "udp:") {
				go btUDPTrackerLoop(ctx, tr, t.hash, feed)
			} else if strings.HasPrefix(tr, "http") {
				go btTrackerLoop(ctx, tr, t.hash, feed)
			}
		}
		if n := dhtN(); n != nil {
			go n.findPeers(ctx, t.hash, feed)
		}
	}

	// ---- 阶段 1：元数据（续传时已在内存，直接跳过重新抓取） ----
	t.mu.Lock()
	meta := t.meta
	t.mu.Unlock()
	if meta == nil {
		// 先查磁盘缓存：同一资源重试/重启后免抓取，秒进下载态
		if m := btMetaCacheGet(t.hash); m != nil {
			meta = m
			t.mu.Lock()
			t.meta = meta
			t.mu.Unlock()
		}
	}
	if meta == nil {
		var errMsg string
		meta, errMsg = btFetchMetaPhase(ctx, t, peerCh)
		if meta == nil {
			if errMsg != "" {
				t.setLock("error", errMsg)
				return "error"
			}
			return t.btStopReason(genAtStart) // ctx 被打断：暂停或停止
		}
		t.mu.Lock()
		t.meta = meta
		t.mu.Unlock()
	}

	// ---- 文件布局进下载态（续传复用已打开布局，不重建文件） ----
	t.mu.Lock()
	full := t.prep
	t.mu.Unlock()
	if full == nil {
		var ferr error
		full, ferr = btPrepareFiles(t, meta)
		if ferr != nil {
			t.setLock("error", ferr.Error())
			return "error"
		}
		t.mu.Lock()
		t.prep = full
		t.have = make([]bool, meta.btNumPieces())
		t.doneN = 0
		t.written = 0
		t.Status = "downloading"
		t.Err = ""
		t.mu.Unlock()
	} else {
		t.mu.Lock()
		t.Status = "downloading"
		t.Err = ""
		t.mu.Unlock()
	}

	// ---- 阶段 2：worker 池 ----
	tech := map[int]bool{} // 已分配未完成片（跨 worker）
	var techMu sync.Mutex
	stall := 0
	total := meta.btNumPieces()
	for {
		if ctx.Err() != nil {
			return t.btStopReason(genAtStart)
		}
		t.mu.Lock()
		alldone := t.doneN >= total
		active := t.peersActive
		noSpeed := t.speed <= 0
		t.mu.Unlock()
		if alldone {
			break
		}
		select {
		case a := <-peerCh:
			addr := a.String()
			stall = 0
			t.mu.Lock()
			if t.peerSeen == nil {
				t.peerSeen = map[string]time.Time{}
			}
			t.peerSeen[addr] = time.Now() // 记入候选池（供持续补拨）
			if last, ok := t.inflight[addr]; ok && time.Since(last) < 30*time.Second {
				t.mu.Unlock()
				continue // 30s 内拨过的地址不重拨
			}
			if need := active < btTuneConns(); !need {
				// 并发已满：不标记 inflight——否则该地址被白跳过 30s，
				// 空位释放后明明有候选却没人可拨（供给断档的帮凶）
				t.mu.Unlock()
				continue
			}
			t.inflight[addr] = time.Now()
			t.mu.Unlock()
			go runWorker(ctx, t, addr, &techMu, tech, full, feed)
		case <-time.After(4 * time.Second):
			stall++
			t.mu.Lock()
			okTech := len(tech) == 0
			// 持续补拨：候选池里 ≥45s 没派过工的地址重新投喂。此前供给完全依赖
			// tracker 60s 突发 + 30s inflight 窗口，拨号失败后连接数塌到个位干等
			// 下一轮突发——这就是连接数频繁大幅跳变（40→1）的根源。
			if active := t.peersActive; active < btTuneConns() && t.testPeers == nil && len(t.peerSeen) > 0 {
				now := time.Now()
				refed := 0
				for a2, last := range t.peerSeen {
					if now.Sub(last) < 45*time.Second {
						continue
					}
					t.peerSeen[a2] = now
					if host, port, serr := net.SplitHostPort(a2); serr == nil {
						if pn, perr := strconv.Atoi(port); perr == nil && pn > 0 {
							feed(btPeerAddr{Host: host, Port: pn})
							refed++
						}
					}
					if refed >= 16 {
						break
					}
				}
				// 池子膨胀控制：只留最近的候选
				if len(t.peerSeen) > 640 {
					for a2, last := range t.peerSeen {
						if now.Sub(last) > 15*time.Minute {
							delete(t.peerSeen, a2)
						}
					}
				}
			}
			t.mu.Unlock()
			// 连接空转 20s：主动补拉一轮 peer（tracker 自身周期 60s，避免长时间零速空转）
			if active == 0 && t.testPeers == nil && stall%5 == 0 && stall <= 25 {
				btReannounce(trackers, t.hash, feed)
			}
			// 3 分钟无速度、无连接、无挂起片 → 停滞收场
			if stall > 45 && noSpeed && active == 0 && okTech {
				t.setLock("error", "长时间无可用 peer，下载停滞（资源冷门或网络受限）")
				return "error"
			}
		}
	}
	// 收尾前让 worker 收手：done 时 worker 常阻塞在 btReadMsg（最长 45s 静默超时），
	// 期间一直持有文件句柄。先打 workCancel（worker 每轮循环都检查 ctx.Done）再等它们退出，
	// 否则 btReleasePrep 关句柄后会与 worker 的写盘竞争（Windows 下文件删不掉）。
	t.mu.Lock()
	wc := t.workCancel
	t.mu.Unlock()
	if wc != nil {
		wc() // 只取消本轮工作 ctx，不影响任务本身（t.cancel 才是应用级终止）
	}
	for i := 0; i < 60; i++ {
		t.mu.Lock()
		active := t.peersActive
		t.mu.Unlock()
		if active == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.setLock("done", "")
	if n := dhtN(); n != nil {
		go n.announceAll(t.hash)
	}
	return "done"
}

// btFetchMetaPhase 阶段1：每 peer 一路并发先到先得 + infohash 核对。
// 返回 (nil,"") 表示被 ctx 打断（暂停/停止，归因交给调用方）；(nil,errMsg) 表示超时失败。
func btFetchMetaPhase(ctx context.Context, t *btTask, peerCh chan btPeerAddr) (*btMeta, string) {
	deadline := time.Now().Add(btMetaTryTimeout)
	tried := map[string]bool{}
	ipTries := map[string]int{} // 同 IP 端口尝试计数（超过 btMetaIPCap 跳过，防假 seeder 农场刷屏）
	whys := map[string]int{}    // 失败原因聚合（写进超时错误消息，用户可自查）
	type btMetaTry struct {
		ok  bool
		m   *btMeta
		why string
	}
	resCh := make(chan btMetaTry, 64)
	inflight := 0
	var meta *btMeta
	for meta == nil && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ""
		case r := <-resCh:
			inflight--
			if r.ok {
				meta = r.m
			} else {
				whys[r.why]++
			}
		case a := <-peerCh:
			addr := a.String()
			host, _, herr := net.SplitHostPort(addr)
			if herr != nil {
				host = addr
			}
			// 快速失败归因（对端重置/拒连）不该继续占并发槽，故上限只限制在拨号数
			if tried[addr] || inflight >= btMetaMaxPeers || ipTries[host] >= btMetaIPCap {
				continue
			}
			tried[addr] = true
			ipTries[host]++
			t.mu.Lock()
			t.seenN = len(tried)
			t.mu.Unlock()
			inflight++
			go func(addr string) {
				defer func() { recover() }()
				m, err := btFetchMetaFromPeer(ctx, addr, t.hash)
				r := btMetaTry{ok: err == nil && m != nil, m: m, why: btMetaWhy(err)}
				select {
				case resCh <- r:
				case <-ctx.Done():
				}
			}(addr)
		case <-time.After(500 * time.Millisecond):
			// 更快轮转：peerCh 空时不再干等 2s，让 resCh 结果尽早回到主循环
		}
	}
	if meta != nil {
		btMetaCachePut(t.hash, meta)
		// 阶段1 已握手的 peer 直接回喂给阶段2：否则要等 tracker 下一轮 60s 才有 peer，
		// 白白空转（这是「元数据到手后进度迟迟不动」的常见原因）。
		for a := range tried {
			if host, port, err := net.SplitHostPort(a); err == nil {
				if pn, perr := strconv.Atoi(port); perr == nil && pn > 0 {
					select {
					case peerCh <- btPeerAddr{Host: host, Port: pn}:
					default:
					}
				}
			}
		}
		return meta, ""
	}
	t.mu.Lock()
	seenN := t.seenN
	t.mu.Unlock()
	dmsg := ""
	if n := dhtN(); n != nil {
		dmsg = fmt.Sprintf("，DHT 节点 %d", n.poolSize())
	}
	wmsg := ""
	if len(whys) > 0 {
		keys := make([]string, 0, len(whys))
		for k := range whys {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s %d", k, whys[k]))
		}
		wmsg = "（失败 " + strings.Join(parts, " / ") + "）"
	}
	if seenN == 0 {
		return nil, fmt.Sprintf("获取种子信息超时：未发现任何 peer%s——该资源可能无做种在线；若 DHT 节点为 0 则是本机网络限制 UDP / tracker 访问", dmsg)
	}
	return nil, fmt.Sprintf("获取种子信息超时：已尝试 peer %d%s%s 但无一成功——资源可能冷门或网络受限", seenN, wmsg, dmsg)
}

// btReannounce 补拉一轮 peer（下载期连接空转时调用，绕过 tracker 60s 周期）
func btReannounce(trackers []string, ih [20]byte, out func(btPeerAddr)) {
	defer func() { recover() }()
	for _, tr := range trackers {
		tr := tr
		go func() {
			defer func() { recover() }()
			if strings.HasPrefix(tr, "udp:") {
				btUDPTrackerAnnounce(tr, ih, out, false)
			} else if strings.HasPrefix(tr, "http") {
				btTrackerAnnounce(tr, ih, out)
			}
		}()
	}
}

// ---------- tracker ----------

// btTrackerLoop 中心 tracker 周期 announce（http/https）
func btTrackerLoop(ctx context.Context, tr string, ih [20]byte, out func(btPeerAddr)) {
	defer func() { recover() }()
	if !strings.HasPrefix(tr, "http") {
		return
	}
	for {
		if ctx.Err() != nil {
			return
		}
		btTrackerAnnounce(tr, ih, out)
		select {
		case <-ctx.Done():
			return
		case <-time.After(60 * time.Second):
		}
	}
}

// btUDPTrackerLoop udp://tracker 周期 announce（BEP15；搜索源返回的 tracker 大多
// 是 udp://，之前全部丢弃导致真机上只靠 http tracker 单腿发现 peer）
func btUDPTrackerLoop(ctx context.Context, tr string, ih [20]byte, out func(btPeerAddr)) {
	defer func() { recover() }()
	first := true
	for {
		if ctx.Err() != nil {
			return
		}
		interval := btUDPTrackerAnnounce(tr, ih, out, first)
		first = false
		if interval < 30 {
			interval = 60
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(interval) * time.Second):
		}
	}
}

const btUdpMagic = uint64(0x41727101980) // BEP15 connect 协议魔数

// btUDPTrackerAnnounce 单次 BEP15 announce（connect 握手 + announce + compact peers）。
// 返回 tracker 建议的 re-announce 间隔（秒）。
func btUDPTrackerAnnounce(tr string, ih [20]byte, out func(btPeerAddr), eventStarted bool) int {
	defer func() { recover() }()
	u, err := url.Parse(strings.TrimSpace(tr))
	if err != nil || u.Scheme != "udp" || u.Host == "" {
		return 0
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "80"
	}
	raddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(host, port))
	if err != nil {
		return 0
	}
	conn, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		return 0
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))

	var tx [4]byte
	crand.Read(tx[:])

	// connect: 魔数(8) + action=0(4) + txid(4)
	req := make([]byte, 16)
	binary.BigEndian.PutUint64(req[0:8], btUdpMagic)
	binary.BigEndian.PutUint32(req[8:12], 0)
	copy(req[12:16], tx[:])
	if _, err := conn.Write(req); err != nil {
		return 0
	}
	resp := make([]byte, 512)
	nr, err := conn.Read(resp)
	if err != nil || nr < 16 {
		return 0
	}
	if action := binary.BigEndian.Uint32(resp[0:4]); action != 0 {
		return 0
	}
	if binary.BigEndian.Uint32(resp[4:8]) != binary.BigEndian.Uint32(tx[:]) {
		return 0
	}
	connID := resp[8:16]

	crand.Read(tx[:])
	// announce: connection_id(8) + action=1(4) + txid(4) + info_hash(20) + peer_id(20)
	// + downloaded(8) + left(8) + uploaded(8) + event(4) + ip(4) + key(4) + num_want(4) + port(2)
	req = make([]byte, 98)
	copy(req[0:8], connID)
	binary.BigEndian.PutUint32(req[8:12], 1)
	copy(req[12:16], tx[:])
	copy(req[16:36], ih[:])
	copy(req[36:56], btSelfID[:])
	binary.BigEndian.PutUint64(req[56:64], 0) // downloaded
	binary.BigEndian.PutUint64(req[64:72], 1) // left
	binary.BigEndian.PutUint64(req[72:80], 0) // uploaded
	event := uint32(0)
	if eventStarted {
		event = 2 // started
	}
	binary.BigEndian.PutUint32(req[80:84], event)
	binary.BigEndian.PutUint32(req[84:88], 0) // ip=0（tracker 自动判）
	var key [4]byte
	crand.Read(key[:])
	copy(req[88:92], key[:])
	binary.BigEndian.PutUint32(req[92:96], 200) // num_want
	binary.BigEndian.PutUint16(req[96:98], uint16(maxI(6881, dhtListenPort)))
	if _, err := conn.Write(req); err != nil {
		return 0
	}
	nr, err = conn.Read(resp)
	if err != nil || nr < 20 {
		return 0
	}
	action := binary.BigEndian.Uint32(resp[0:4])
	if binary.BigEndian.Uint32(resp[4:8]) != binary.BigEndian.Uint32(tx[:]) {
		return 0
	}
	if action == 3 { // tracker 报错（截断 infohash 等），静默跳过
		return 0
	}
	if action != 1 {
		return 0
	}
	interval := int(binary.BigEndian.Uint32(resp[8:12]))
	// resp[12:16] leechers, resp[16:20] seeders（未用）
	for i := 20; i+6 <= nr; i += 6 {
		out(decodeCompactPeer(string(resp[i : i+6])))
	}
	return interval
}

// btTrackerAnnounce 单次 compact announce
func btTrackerAnnounce(tr string, ih [20]byte, out func(btPeerAddr)) {
	defer func() { recover() }()
	u, err := url.Parse(strings.TrimSpace(tr))
	if err != nil || u.Host == "" {
		return
	}
	q := u.Query()
	q.Set("info_hash", string(ih[:]))
	q.Set("peer_id", string(btSelfID[:]))
	q.Set("port", strconv.Itoa(maxI(6881, dhtListenPort)))
	q.Set("uploaded", "0")
	q.Set("downloaded", "0")
	q.Set("left", "1")
	q.Set("compact", "1")
	q.Set("event", "started")
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(context.Background(), "GET", u.String(), nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", dlDefaultUA)
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	v, _, err := btDec(b)
	if err != nil {
		return
	}
	d, isMap := v.(map[string]any)
	if !isMap {
		return
	}
	if p, ok := d["peers"].(string); ok {
		for i := 0; i+6 <= len(p); i += 6 {
			out(decodeCompactPeer(p[i : i+6]))
		}
	}
	if p6, ok := d["peers6"].(string); ok { // IPv6 compact peers（BEP7）
		for i := 0; i+18 <= len(p6); i += 18 {
			out(decodeCompactPeerV6(p6[i : i+18]))
		}
	}
	if lst, ok := d["peers"].([]any); ok {
		for _, e := range lst {
			if pd, ok := e.(map[string]any); ok {
				ip := btStr(pd, "ip")
				pt := int(btInt(pd, "port"))
				if ip != "" && pt > 0 {
					out(btPeerAddr{Host: ip, Port: pt})
				}
			}
		}
	}
}

func maxI(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// btMetaWhy 元数据抓取失败原因归类（聚合后写进任务错误消息，帮助用户自查网络）
func btMetaWhy(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "forcibly closed"):
		return "对端重置"
	case strings.Contains(s, "connection refused"):
		return "拒连"
	case strings.Contains(s, "EOF"):
		return "断开"
	case strings.Contains(s, "broken pipe"):
		return "断开"
	case strings.Contains(s, "timeout") || strings.Contains(s, "timeout"):
		return "超时"
	case strings.Contains(s, "不支持"):
		return "不支持元数据"
	case strings.Contains(s, "不匹配"):
		return "异种"
	case strings.Contains(s, "拒绝"):
		return "拒回"
	case strings.Contains(s, "异常"):
		return "异常"
	}
	return "其他"
}

// ---------- 文件布局 ----------

type btFileOut struct {
	off    int64
	size   int64
	handle *os.File
}

type btPrepared struct {
	files []btFileOut
}

// btPrepareFiles 建目录 + 预创建文件（单文件 / 多文件两种布局）
func btPrepareFiles(t *btTask, meta *btMeta) (*btPrepared, error) {
	if total := meta.btTotalSize(); total > btMaxTorrentSz {
		return nil, fmt.Errorf("种子总大小 %dMB 超过上限（%dMB）", total>>20, btMaxTorrentSz>>20)
	}
	var off int64
	var outs []btFileOut
	reuse := t.reuseFiles // 断点恢复：沿用原文件名（否则每次重启都会多一个「xxx (1)」）
	if len(meta.Files) == 0 {
		base := btCleanName(meta.Name)
		if base == "" {
			return nil, fmt.Errorf("种子文件名异常")
		}
		path := filepath.Join(t.Dir, base)
		if !reuse {
			path = dlMusUniquePath(t.Dir, base)
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, fmt.Errorf("创建文件失败：%s", err.Error())
		}
		outs = append(outs, btFileOut{off: 0, size: meta.Len, handle: f})
		return &btPrepared{files: outs}, nil
	}
	sub := filepath.Join(t.Dir, btCleanName(meta.Name))
	if err := os.MkdirAll(sub, 0o755); err != nil {
		return nil, err
	}
	for _, sf := range meta.Files {
		path := filepath.Join(sub, filepath.FromSlash(btCleanRel(sf.Path)))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		if sf.Size > 0 {
			f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return nil, err
			}
			outs = append(outs, btFileOut{off: off, size: sf.Size, handle: f})
		}
		off += sf.Size
	}
	if len(outs) == 0 {
		return nil, fmt.Errorf("种子文件列表异常")
	}
	return &btPrepared{files: outs}, nil
}

// btCleanRel 多文件相对路径逐段清洗
func btCleanRel(p string) string {
	var out []string
	for _, seg := range strings.Split(p, "/") {
		if seg = btCleanName(seg); seg != "" {
			out = append(out, seg)
		}
	}
	return strings.Join(out, "/")
}

// btWritePiece 已校验片 → 布局写入（可跨文件）
func (p *btPrepared) writePiece(pieceIdx int, pieceLen int64, data []byte) error {
	gOff := int64(pieceIdx) * pieceLen
	gEnd := gOff + int64(len(data))
	for _, f := range p.files {
		fEnd := f.off + f.size
		if fEnd <= gOff || f.off >= gEnd {
			continue
		}
		start := gOff
		if start < f.off {
			start = f.off
		}
		end := gEnd
		if end > fEnd {
			end = fEnd
		}
		if _, err := f.handle.WriteAt(data[start-gOff:end-gOff], start-f.off); err != nil {
			return err
		}
	}
	return nil
}

// btCloseAllPrep 收尾关文件句柄
func (p *btPrepared) btCloseAllPrep() {
	for _, f := range p.files {
		f.handle.Close()
	}
}

// ---------- 下载 worker ----------

// runWorker 单连接 worker：握手 → 扩展握手 → interested → 顺序抢片要块
func runWorker(ctx context.Context, t *btTask, addr string, techMu *sync.Mutex, tech map[int]bool, full *btPrepared, feed func(btPeerAddr)) {
	defer func() { recover() }()
	t.mu.Lock()
	meta := t.meta
	t.peersActive++
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.peersActive--
		t.mu.Unlock()
	}()
	if meta == nil {
		return
	}
	dialer := &net.Dialer{Timeout: 8 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return
	}
	connStart := time.Now() // 连接建立时刻（choke 超时从此刻起算）
	defer conn.Close()
	if _, err := btHandshake(conn, t.hash); err != nil {
		return
	}
	if err := btWriteMsg(conn, 20, btJoinExt(0, map[string]any{
		"m": map[string]any{"ut_metadata": 1},
		"p": maxI(6881, dhtListenPort), "v": "X-Box " + version,
	}, nil)); err != nil {
		return
	}
	if err := btWriteMsg(conn, 2, nil); err != nil { // interested
		return
	}
	// 协议链路（握手→扩展握手→interested）全通才算「可用连接」：
	// 此前展示的是含拨号尝试的 peersActive，tracker 每 60s 突发供给一轮
	// 候选地址会先冲高再随失败塌落，前端表现为连接数频繁大幅跳变（40→1）。
	t.mu.Lock()
	t.peersReady++
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.peersReady--
		t.mu.Unlock()
	}()
	var peerBF []bool
	curPiece := -1
	curBuf := []byte{}
	got := 0               // 该片已收字节数（判定整片收齐，避免 sent 重扫把已收块再请求一遍）
	sent := map[int]bool{} // 该片已请求未回的块偏移
	recv := map[int]bool{} // 该片已收到的块偏移
	holding := false
	pexID := byte(0) // 对端 ut_pex 消息 id（0=未知，收到其扩展握手后记下）
	choked := true   // 协议规定连接初始为 choke 态，收到 unchoke 才能取数据
	chokedAt := time.Time{}
	lastProg := time.Now() // 最近一次该连接收到新字节的时间（零进展看门狗）
	endgame := false       // 端游竞速：当前 curPiece 是否为竞争片（不设/不删 tech，允许多 worker 抢同尾片）
	defer func() {         // worker 退出释放挂起片
		if holding && curPiece >= 0 && !endgame {
			techMu.Lock()
			delete(tech, curPiece)
			techMu.Unlock()
		}
	}()
	pendAt := time.Now()
	idleRound := 0
	// 协议规定连接初始为 choke；做种者合法地不主动发 choke(0)，若用零值判断超时
	// 会永不触发，被 choke 的连接挂着不死、占满并发槽 → 新 peer 拨不进来（连接数
	// 长期 1~2 的直接原因）。改为从连接建立起计时。
	chokedAt = connStart
	nextInterest := time.Now()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		typ, pl, err := btReadMsg(conn)
		if err != nil {
			return // 45s 静默 / 断开 / 对端失联，worker 自然退场
		}
		switch typ {
		case 0xFF: // keep-alive
			if holding && time.Since(pendAt) > btStalePiece {
				if time.Since(lastProg) > 3*time.Minute {
					// 重发多轮仍零进展：这条 peer 养不熟这片，释放让别的连接接手
					if !endgame {
						techMu.Lock()
						delete(tech, curPiece)
						techMu.Unlock()
					}
					holding, curPiece, curBuf, sent, recv, got = false, -1, nil, map[int]bool{}, map[int]bool{}, 0
				} else {
					// 只重发未回的块，已收的（recv/curBuf）全保留——此前整片丢弃重收，
					// 40s 一轮、慢速 peer 永远凑不齐一个片（下载卡 0% 的根因）
					sent = map[int]bool{}
					pendAt = time.Now()
				}
			}
			continue
		case 0: // choke（记录状态后穿透到选片区；continue 会与「对端等 request」互等死锁）
			choked, chokedAt = true, time.Now()
		case 1: // unchoke（同上，穿透：收到 unchoke 后立即派发请求）
			choked = false
		case 20: // 扩展消息：记对端 ut_pex id + 消费 PEX 新 peer（下载态此前直接丢弃）
			if len(pl) >= 1 {
				if mid := pl[0]; mid == 0 {
					if d, _, err := btSplitExt(pl); err == nil {
						if m2, ok2 := d["m"].(map[string]any); ok2 {
							if f, ok3 := m2["ut_pex"].(int64); ok3 && f > 0 && f < 255 {
								pexID = byte(f)
							}
						}
					}
				} else if mid == pexID {
					btConsumePEX(pl[1:], feed)
				}
			}
			continue
		case 4: // have
			if len(pl) == 4 && peerBF != nil {
				idx := int(pl[0])<<24 | int(pl[1])<<16 | int(pl[2])<<8 | int(pl[3])
				if idx >= 0 && idx < len(peerBF) {
					peerBF[idx] = true
				}
			}
		case 5: // bitfield
			if peerBF == nil && len(pl) > 0 {
				peerBF = make([]bool, meta.btNumPieces())
				for i, b := range pl {
					if i*8 >= len(peerBF) {
						break
					}
					for bit := 0; bit < 8; bit++ {
						if b&(0x80>>bit) != 0 && i*8+bit < len(peerBF) {
							peerBF[i*8+bit] = true
						}
					}
				}
			}
			continue
		case 7: // piece 块：index(4)+begin(4)+data
			if len(pl) < 8 || curPiece < 0 {
				continue
			}
			idx := int(pl[0])<<24 | int(pl[1])<<16 | int(pl[2])<<8 | int(pl[3])
			begin := int(pl[4])<<24 | int(pl[5])<<16 | int(pl[6])<<8 | int(pl[7])
			data := pl[8:]
			if idx != curPiece || begin < 0 || begin+len(data) > len(curBuf) || begin%btBlockLen != 0 {
				continue
			}
			if recv[begin] { // 重复块直接丢弃
				continue
			}
			recv[begin] = true
			delete(sent, begin)
			copy(curBuf[begin:], data)
			got += len(data)
			pendAt = time.Now()
			lastProg = time.Now()
			if got >= len(curBuf) { // 整片收齐 → SHA1 校验 → 落盘
				sum := sha1.Sum(curBuf)
				if sum == meta.btPieceSha1(idx) {
					if werr := full.writePiece(idx, meta.PieceLen, curBuf); werr == nil {
						t.mu.Lock()
						if !t.have[idx] {
							t.have[idx] = true
							t.doneN++
							t.written += int64(len(curBuf))
						}
						t.mu.Unlock()
					}
				}
				if !endgame { // 独占 worker 才释放占用；端游竞速 worker 未设过 tech，删了会误删独占锁
					techMu.Lock()
					delete(tech, idx)
					techMu.Unlock()
				}
				holding, curPiece, curBuf, sent, recv, got = false, -1, nil, map[int]bool{}, map[int]bool{}, 0
			}
		}
		// 选片 / 派块（bitfield 到手才能干活）——choke 期间仍要持续读消息以便
		// 捕获对端放行的 unchoke，不能阻塞在 select/sleep。
		if peerBF == nil {
			continue
		}
		// 定期重申 interested（~25s）：做种者乐观放行轮换的「候选提醒」。
		// 只发一次的话等轮换轮到自己前对端早把我们当惰性下者，重申能留在其放行池。
		if choked && time.Since(nextInterest) > 25*time.Second {
			if btWriteMsg(conn, 2, nil) != nil { // interested
				return
			}
			nextInterest = time.Now()
		}
		if choked && time.Since(chokedAt) > btUnchokeWait {
			// 等 unchoke 超过窗口仍被 choke：让出并发槽去拨别的 peer
			return
		}
		if choked {
			// 被 choke：释放已持有的片（本连接传不了数据，片让给能传的 worker），
			// 但仍保持连接、挂到下次循环走重申 interested 的路径，等对端放行。
			if holding && curPiece >= 0 && !endgame {
				techMu.Lock()
				delete(tech, curPiece)
				techMu.Unlock()
				holding, curPiece, curBuf, sent, recv, got = false, -1, nil, map[int]bool{}, map[int]bool{}, 0
			}
			// 不选新片、不派块，直接继续读等放行
			continue
		}
		// 端游竞速片若已被别的 worker 拼齐写盘（have[curPiece] 变 true）：立即放弃，
		// 重新选片，不再为已完成的片浪费带宽。
		if endgame && curPiece >= 0 {
			t.mu.Lock()
			donep := false
			if curPiece < len(t.have) && t.have[curPiece] {
				donep = true
			}
			t.mu.Unlock()
			if donep {
				endgame = false
				holding, curPiece, curBuf, sent, recv, got = false, -1, nil, map[int]bool{}, map[int]bool{}, 0
			}
		}
		if curPiece < 0 {
			endgame = false // 先按独占短想：下面是独占成功则保持 false；端游路径才置 true
			np, ok := btNextPiece(t, techMu, tech, meta, peerBF)
			if !ok {
				t.mu.Lock()
				alldone := t.doneN >= meta.btNumPieces()
				t.mu.Unlock()
				if alldone {
					return
				}
				// 端游竞争：独占没片但任务还没完（最后 1~2 片被慢 peer 独占）→
				// 让空闲 worker 竞速尾片，谁先拼齐谁写盘（写盘幂等）。否则空闲
				// worker 只能 idleRound 空等 2s×60 后退场，快 peer 帮不上忙。
				if ep, eok := btPickEndgame(t, meta, peerBF); eok {
					np, ok, endgame = ep, true, true
					idleRound = 0
				} else {
					idleRound++
					if idleRound > 60 { // 这条 peer 没有我们缺的块，退场让位
						return
					}
					time.Sleep(2 * time.Second)
					continue
				}
			}
			if !(np >= 0) {
				idleRound++
				if idleRound > 60 {
					return
				}
				time.Sleep(2 * time.Second)
				continue
			}
			idleRound = 0
			curPiece, holding = np, true
			plLen := meta.PieceLen
			if np == meta.btNumPieces()-1 {
				plLen = meta.btTotalSize() - int64(np)*meta.PieceLen
			}
			curBuf = make([]byte, plLen)
			sent = map[int]bool{}
			recv = map[int]bool{}
			got = 0
			pendAt = time.Now()
		}
		// 挂起块（管线尺寸内尽量填满）——已收（recv）与挂起（sent）都不再请求
		pipe := btTunePipeline()
		if len(sent) >= pipe {
			if time.Since(pendAt) > btStalePiece { // 全挂起但长期无返回
				if time.Since(lastProg) > 3*time.Minute {
					// 零进展看门狗：释放该片，别的连接接手
					if !endgame {
						techMu.Lock()
						delete(tech, curPiece)
						techMu.Unlock()
					}
					holding, curPiece, curBuf, sent, recv, got = false, -1, nil, map[int]bool{}, map[int]bool{}, 0
				} else {
					// 只重发未回的块、保留已收（与 keep-alive 分支同策略，防慢 peer 死循环）
					sent = map[int]bool{}
					pendAt = time.Now()
				}
			}
		} else {
			for off := 0; off < len(curBuf) && len(sent) < pipe; off = nextBlockOff(curBuf, off) {
				if sent[off] || recv[off] {
					continue
				}
				if btReqBlock(conn, int64(curPiece), int64(off), int64(len(curBuf)-off)) != nil {
					return
				}
				sent[off] = true
			}
		}
	}
}

// nextBlockOff 下一块请求偏移：末块可能短于标准块长（一次请求吃满余下）
func nextBlockOff(curBuf []byte, off int) int {
	n := off + btBlockLen
	if n >= len(curBuf) {
		return len(curBuf) // 终止
	}
	return n
}

// btConsumePEX 解析 ut_pex 载荷（bencode dict，added 字段 = 6 字节紧凑条目×N），
// 把新 peer 投喂进候选通道。已连接 peer 是公网种子的重要 peer 来源，
// 此前下载态直接丢弃扩展消息，供给只剩 tracker/DHT。
func btConsumePEX(payload []byte, feed func(btPeerAddr)) {
	if len(payload) == 0 || len(payload) > 4096 {
		return
	}
	v, _, err := btDec(payload)
	if err != nil {
		return
	}
	d, ok := v.(map[string]any)
	if !ok {
		return
	}
	added, ok := d["added"].(string)
	if !ok || len(added) < 6 {
		return
	}
	n := len(added) / 6
	if n > 32 { // 单帧上限，防刷屏
		n = 32
	}
	for i := 0; i < n; i++ {
		b := added[i*6 : i*6+6]
		port := int(b[4])<<8 | int(b[5])
		if port <= 0 {
			continue
		}
		ip := net.IPv4(b[0], b[1], b[2], b[3])
		if ip.IsUnspecified() || ip.IsLoopback() {
			continue
		}
		feed(btPeerAddr{Host: ip.String(), Port: port})
	}
}

// btReqBlock 发送 request(index, offset, len)
func btReqBlock(conn net.Conn, piece, off, plen int64) error {
	if plen > btBlockLen {
		plen = btBlockLen
	}
	req := make([]byte, 12)
	req[0] = byte(piece >> 24)
	req[1] = byte(piece >> 16)
	req[2] = byte(piece >> 8)
	req[3] = byte(piece)
	req[4] = byte(off >> 24)
	req[5] = byte(off >> 16)
	req[6] = byte(off >> 8)
	req[7] = byte(off)
	req[8] = byte(plen >> 24)
	req[9] = byte(plen >> 16)
	req[10] = byte(plen >> 8)
	req[11] = byte(plen)
	return btWriteMsg(conn, 6, req)
}

// btNextPiece 顺序选片：找第一个「我缺 & 对方有 & 未挂起」的片
func btNextPiece(t *btTask, techMu *sync.Mutex, tech map[int]bool, meta *btMeta, peerBF []bool) (int, bool) {
	t.mu.Lock()
	have := t.have
	t.mu.Unlock()
	techMu.Lock()
	defer techMu.Unlock()
	for i := 0; i < meta.btNumPieces(); i++ {
		if i < len(have) && !have[i] && peerBF[i] && !tech[i] {
			tech[i] = true
			return i, true
		}
	}
	return -1, false
}

// btPickEndgame 端游竞争选片：独占 worker 拿不到空缺片（最后 1~2 片被慢 peer 独占）
// 时，让空闲 worker 竞速这些未完成尾片。**不设 tech[i]**（允许多 worker 抢同片），
// 各自从自己的 peer 拉整片，谁先拼齐谁写盘（写盘幂等，have 互斥防 doneN 重复）。
// 这样快 peer 不必再等被慢 peer 独占的尾片，收尾不再越下越慢。
func btPickEndgame(t *btTask, meta *btMeta, peerBF []bool) (int, bool) {
	t.mu.Lock()
	have := t.have
	doneN := t.doneN
	t.mu.Unlock()
	if meta.btNumPieces()-doneN > btEndgamePieces { // 未进入尾段，不做端游竞争
		return -1, false
	}
	for i := 0; i < meta.btNumPieces(); i++ {
		if i < len(have) && !have[i] && peerBF[i] {
			return i, true
		}
	}
	return -1, false
}

// ---------- 对外（dlapi 调） ----------

// btFind 按 id 找任务
func btFind(id string) *btTask {
	btInit()
	btStore.mu.Lock()
	defer btStore.mu.Unlock()
	return btStore.tasks[id]
}

// btPause 暂停任务：打断本轮工作并让出并发槽，保留 meta/have/prep（已落盘分片 +
// 文件句柄），恢复时从断点续传。返回 false 表示当前状态不可暂停（终态）。
func btPause(id string) bool {
	t := btFind(id)
	if t == nil {
		return false
	}
	t.mu.Lock()
	st := t.Status
	if !btActiveStatus(st) {
		t.mu.Unlock()
		return st == "paused"
	}
	t.paused = true
	t.pauseGen++ // 标记本轮被暂停打断（即使随后立刻 resume，也不会误判为 stopped）
	t.Status = "paused"
	t.Err = ""
	t.speed = 0
	wc := t.workCancel
	t.mu.Unlock()
	if wc != nil {
		wc() // 打断本轮（worker/tracker/DHT 都挂在轮次 ctx 上），runTask 退出并释放槽位
	}
	return true
}

// btResume 继续任务：重新拉起一轮工作，沿用已有 meta/have/prep → 断点续传
func btResume(id string) bool {
	t := btFind(id)
	if t == nil {
		return false
	}
	t.mu.Lock()
	if t.Status != "paused" {
		t.mu.Unlock()
		return false
	}
	t.paused = false
	t.Err = ""
	t.speed = 0
	// 立刻回到运行态（不必等槽位），列表即时反馈
	if t.meta != nil {
		t.Status = "downloading"
	} else {
		t.Status = "fetching"
	}
	t.mu.Unlock()
	btStartWork(t)
	return true
}

// btCancel 取消/停止任务（终态 stopped，释放文件句柄，不保留续传能力）
func btCancel(id string) {
	btInit()
	btStore.mu.Lock()
	t := btStore.tasks[id]
	btStore.mu.Unlock()
	if t == nil {
		return
	}
	t.setLock("stopped", "")
	t.mu.Lock()
	t.paused = false
	wc := t.workCancel
	t.mu.Unlock()
	if wc != nil {
		wc()
	}
	if t.cancel != nil {
		t.cancel()
	}
	btReleasePrep(t)
}

// btActiveStatus 运行态判定（可暂停/可停止）
func btActiveStatus(st string) bool {
	return st == "pending" || st == "fetching" || st == "downloading"
}

// btPauseAll 全部暂停（只作用于运行中任务），返回受影响数量
func btPauseAll() int {
	btInit()
	ids := btAllIDs()
	n := 0
	for _, id := range ids {
		if btPause(id) {
			n++
		}
	}
	return n
}

// btResumeAll 全部继续，返回受影响数量
func btResumeAll() int {
	btInit()
	ids := btAllIDs()
	n := 0
	for _, id := range ids {
		if btResume(id) {
			n++
		}
	}
	return n
}

// btCancelAllTasks 全部停止（运行中 + 暂停中的都停），返回受影响数量
func btCancelAllTasks() int {
	btInit()
	ids := btAllIDs()
	n := 0
	for _, id := range ids {
		if t := btFind(id); t != nil {
			t.mu.Lock()
			st := t.Status
			t.mu.Unlock()
			if btActiveStatus(st) || st == "paused" {
				btCancel(id)
				n++
			}
		}
	}
	return n
}

// btAllIDs 任务 id 快照
func btAllIDs() []string {
	btStore.mu.Lock()
	defer btStore.mu.Unlock()
	out := make([]string, 0, len(btStore.order))
	out = append(out, btStore.order...)
	return out
}

// btRemove 移除终态任务（运行中/暂停中不可移除：先停止）
func btRemove(id string) {
	btInit()
	btStore.mu.Lock()
	t, ok := btStore.tasks[id]
	if ok {
		t.mu.Lock()
		st := t.Status
		t.mu.Unlock()
		if btActiveStatus(st) || st == "paused" {
			ok = false
		}
	}
	if ok {
		delete(btStore.tasks, id)
		for i, x := range btStore.order {
			if x == id {
				btStore.order = append(btStore.order[:i], btStore.order[i+1:]...)
				break
			}
		}
	}
	btStore.mu.Unlock()
}

// btTaskFiles 算出某任务落盘的文件绝对路径（删除已下载文件用）。
// 只返回**该任务目录内**的产物：单文件=dir/name；多文件=dir/name/子路径。
// 目录外的路径一律丢弃（防止历史脏数据把用户其它文件删掉）。
func btTaskFiles(t *btTask) []string {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	meta := t.meta
	dir := t.Dir
	t.mu.Unlock()
	if meta == nil || dir == "" {
		return nil
	}
	dir = filepath.Clean(dir)
	var out []string
	safeJoin := func(rel string) string {
		p := filepath.Clean(filepath.Join(dir, rel))
		// 必须仍在任务目录内（防 ../ 逃逸）
		if p != dir && !strings.HasPrefix(p, dir+string(os.PathSeparator)) {
			return ""
		}
		return p
	}
	if len(meta.Files) == 0 {
		if base := btCleanName(meta.Name); base != "" {
			if p := safeJoin(base); p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	sub := btCleanName(meta.Name)
	for _, sf := range meta.Files {
		rel := filepath.FromSlash(btCleanRel(sf.Path))
		if p := safeJoin(filepath.Join(sub, rel)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// btRemoveWithFiles 移除任务，deleteFiles=true 时同时删除已下载的文件（及空目录）。
// 返回被删除的文件数量。只删普通文件，绝不递归删非空目录。
func btRemoveWithFiles(id string, deleteFiles bool) (removed int, err error) {
	t := btFind(id)
	if t == nil {
		return 0, nil
	}
	t.mu.Lock()
	st := t.Status
	t.mu.Unlock()
	// 运行中/暂停中必须先停止（避免边下边删）
	if btActiveStatus(st) || st == "paused" {
		return 0, fmt.Errorf("任务仍在运行，请先停止")
	}
	var files []string
	if deleteFiles {
		files = btTaskFiles(t)
	}
	btRemove(id)
	if !deleteFiles {
		return 0, nil
	}
	for _, p := range files {
		fi, serr := os.Lstat(p)
		if serr != nil {
			continue // 不存在即跳过
		}
		if fi.IsDir() || !fi.Mode().IsRegular() {
			continue // 只删普通文件
		}
		if rerr := os.Remove(p); rerr == nil {
			removed++
		}
	}
	// 顺带清理空目录（只删空目录，非空一律保留）
	if len(files) > 0 {
		t.mu.Lock()
		dir := t.Dir
		meta := t.meta
		t.mu.Unlock()
		if meta != nil && len(meta.Files) > 0 && dir != "" {
			sub := filepath.Join(dir, btCleanName(meta.Name))
			pruneEmptyDirs(sub)
		}
	}
	return removed, nil
}

// btClearDone 清除已完成任务，返回清除数量
func btClearDone() int {
	btInit()
	btStore.mu.Lock()
	defer btStore.mu.Unlock()
	n := 0
	keep := btStore.order[:0]
	for _, id := range btStore.order {
		t := btStore.tasks[id]
		st := ""
		if t != nil {
			t.mu.Lock()
			st = t.Status
			t.mu.Unlock()
		}
		if st == "done" {
			delete(btStore.tasks, id)
			n++
			continue
		}
		keep = append(keep, id)
	}
	btStore.order = keep
	return n
}

// btClearAll 清空任务列表（运行中先停止），返回清除数量
func btClearAll() int {
	btInit()
	btCancelAllTasks()
	btStore.mu.Lock()
	n := len(btStore.tasks)
	btStore.tasks = map[string]*btTask{}
	btStore.order = nil
	btStore.mu.Unlock()
	return n
}

// btCancelAll 关停全部（测试用）
func btCancelAll() {
	btInit()
	btStore.mu.Lock()
	copy := make([]*btTask, 0, len(btStore.tasks))
	for _, t := range btStore.tasks {
		copy = append(copy, t)
	}
	btStore.mu.Unlock()
	for _, t := range copy {
		if t.cancel != nil {
			t.cancel()
		}
	}
}

// DlTaskView 统一任务视图（BT + 音乐）
type DlTaskView struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"` // video / music
	Sub       string    `json:"sub"`  // bt / netease / kuwo
	Title     string    `json:"title"`
	Status    string    `json:"status"`
	Progress  float64   `json:"progress"`
	Done      int64     `json:"done"`
	Total     int64     `json:"total"`
	Speed     int64     `json:"speed"`
	Peers     int       `json:"peers"`
	Dir       string    `json:"dir,omitempty"`
	Path      string    `json:"path,omitempty"`
	Err       string    `json:"err,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// btViews BT 任务视图
func btViews() []DlTaskView {
	btInit()
	btStore.mu.Lock()
	defer btStore.mu.Unlock()
	out := make([]DlTaskView, 0, len(btStore.order))
	for _, id := range btStore.order {
		t := btStore.tasks[id]
		if t == nil {
			continue
		}
		t.mu.Lock()
		v := DlTaskView{
			ID: t.ID, Type: "video", Sub: "bt",
			Title: t.Title, Status: t.Status, Err: t.Err,
			CreatedAt: t.CreatedAt, Dir: t.Dir,
			Speed: t.speed, Done: t.written,
		}
		if t.Status == "fetching" {
			v.Peers = t.seenN // fetching 态展示「已发现的候选 peer 数」
		} else {
			v.Peers = t.peersReady // 只数握手成功的真实连接（拨号尝试不算，否则随 tracker 突发供给脉冲式 40→1 跳变）
		}
		if t.meta != nil {
			v.Total = t.meta.btTotalSize()
			if v.Total > 0 {
				v.Progress = float64(t.written*100) / float64(v.Total)
			}
		}
		if t.Status == "done" {
			v.Progress = 100
			if v.Total <= 0 {
				v.Total = t.written
			}
		}
		if v.Progress > 100 {
			v.Progress = 100
		}
		t.mu.Unlock()
		out = append(out, v)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].CreatedAt.After(out[b].CreatedAt) })
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

// dlUnifiedTasks 统一任务列表（BT + 音乐直链任务）
func dlUnifiedTasks() []DlTaskView {
	out := btViews()
	for _, t := range dlMusTaskList() {
		sub := "netease"
		if t.Source == "kuwo" {
			sub = "kuwo"
		}
		status := t.Status
		if status == "ok" {
			status = "done"
		}
		tv := DlTaskView{
			ID: t.ID, Type: "music", Sub: sub,
			Title:  strings.TrimSpace(t.Artist + " - " + t.Song),
			Status: status,
			Done:   t.Bytes, Total: t.Totals,
			Path: t.Path, Dir: t.Dir, Err: t.Err, CreatedAt: t.At,
		}
		if tv.Total > 0 {
			tv.Progress = float64(tv.Done*100) / float64(tv.Total)
		}
		if tv.Status == "done" {
			tv.Progress = 100
			if tv.Total <= 0 {
				tv.Total = tv.Done
			}
		}
		if tv.Progress > 100 {
			tv.Progress = 100
		}
		out = append(out, tv)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].CreatedAt.After(out[b].CreatedAt) })
	return out
}

// 磁力提交（dlapi 调）
func dlStartBT(magnet, dir, title string) (*btTask, error) {
	t, _, err := dlStartBTDedup(magnet, dir, title)
	return t, err
}

// dlStartBTDedup 提交磁力并告知是否命中已有任务（dup=true 表示没有新建，复用了既有任务）
func dlStartBTDedup(magnet, dir, title string) (*btTask, bool, error) {
	if strings.TrimSpace(dir) == "" {
		dir = dlDefaultDir("video")
	}
	return btCreateDedup(magnet, dir, title, nil)
}
