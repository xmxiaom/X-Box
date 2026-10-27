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
	"time"
)

const (
	btMaxTasks       = 8  // 引擎同时任务数
	btMaxConns       = 10 // 单种并发下载连接
	btMetaTryTimeout = 3 * time.Minute // 元数据阶段总窗口（DHT 冷启动/冷门做种需要余地）
	btMetaMaxPeers   = 12              // 元数据阶段并发拨号上限（先到先得）
	btMetaIPCap      = 3               // 同一 IP 最多尝试的端口数（防假 seeder 农场占满并发槽）
	btMaxTrackers    = 20              // 合并磁力自带(含 udp://) + 公共 tracker 的上限
	btMaxTorrentSz   = int64(4) << 30
	btBlockLen       = 16 << 10 // 分块 16KB
	btPipelineBlocks = 8        // 单连接挂起块数
	btStalePiece     = 40 * time.Second
	btPeersChCap     = 256
)

// btDefaultTrackers 公共 tracker 兜底：磁力自带 tr 列表薄时保证 peer 发现半径。
// 只用 http/https（btTrackerLoop 不实现 udp tracker）。
var btDefaultTrackers = []string{
	"http://tracker.opentrackr.org:1337/announce",
	"http://tracker.openbittorrent.com:6969/announce",
	"http://exodus.desync.com:6969/announce",
	"http://open.tracker.cl:1337/announce",
	"https://tracker.tamersunion.org:443/announce",
	"http://tracker.bt4g.com:2095/announce",
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
	seenN       int                 // 已发现的候选 peer 数（fetching 态诊断展示）
	inflight    map[string]time.Time // 最近连过的 peer（30s 内不重复拨）

	cancel context.CancelFunc
	ctx    context.Context

	testPeers []string // 测试注入：只直连这些地址（跳过 tracker/DHT）
}

var btStore struct {
	mu    sync.Mutex
	tasks map[string]*btTask
	order []string
	sema  chan struct{}
	once  sync.Once
}

func btInit() {
	btStore.once.Do(func() {
		btStore.tasks = map[string]*btTask{}
		btStore.sema = make(chan struct{}, btMaxTasks)
		dhtInit()
	})
}

// btCreate 入队（同种子重复点击返回已有任务）
func btCreate(magnet, dir, title string) (*btTask, error) {
	return btCreateEx(magnet, dir, title, nil)
}

// btCreateEx 变体：testPeers 非 nil 时只直连这些地址（单测 fake seeder 用）
func btCreateEx(magnet, dir, title string, testPeers []string) (*btTask, error) {
	btInit()
	hash, dn, trackers, err := btMagnet(magnet)
	if err != nil {
		return nil, err
	}
	if _, safe := safeDownloadDir(dir); !safe {
		return nil, fmt.Errorf("下载目录不可用：%s", dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建目录失败：%s", err.Error())
	}
	// 同种子（同 infohash）已在队列：直接返回既有任务，不重复入队
	btStore.mu.Lock()
	for _, ex := range btStore.tasks {
		if ex.hash == hash {
			st := ex.Status
			// 已进终态的旧任务允许重新创建（覆盖为新任务）
			if st != "error" && st != "stopped" {
				btStore.mu.Unlock()
				return ex, nil
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
	id := "bt" + btHashHex(hash)[:12] + strconv.FormatInt(time.Now().UnixNano()%1e6, 10)
	t := &btTask{
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

	select {
	case btStore.sema <- struct{}{}:
		go func() {
			defer func() { <-btStore.sema }()
			runTask(t, trackers)
		}()
	default:
		go func() {
			select {
			case btStore.sema <- struct{}{}:
				runTask(t, trackers)
			case <-t.ctx.Done():
			case <-time.After(30 * time.Minute):
				t.setLock("error", "排队超时（同时任务过多）")
			}
		}()
	}
	return t, nil
}

func hashHexShort(h [20]byte) string { return btHashHex(h)[:8] }

// setLock 锁内置终态
func (t *btTask) setLock(status, errMsg string) {
	t.mu.Lock()
	t.Status = status
	t.Err = errMsg
	t.mu.Unlock()
}

// runTask 主流程
func runTask(t *btTask, trackers []string) {
	defer func() {
		if r := recover(); r != nil {
			t.setLock("error", "内部错误："+fmt.Sprint(r))
		}
	}()
	ctx := t.ctx
	t.setLock("fetching", "")

	// 速度打点：每秒计算 written 差值（字节/秒）
	go func() {
		defer func() { recover() }()
		tk := time.NewTicker(1 * time.Second)
		defer tk.Stop()
		t.mu.Lock()
		prev := t.written
		t.mu.Unlock()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				t.mu.Lock()
				t.speed = t.written - prev
				prev = t.written
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

	// ---- 阶段 1：元数据（每 peer 一路并发先到先得 + infohash 核对） ----
	var meta *btMeta
	deadline := time.Now().Add(btMetaTryTimeout)
	tried := map[string]bool{}
	ipTries := map[string]int{} // 同 IP 端口尝试计数（超过 btMetaIPCap 跳过，防假 seeder 农场刷屏）
	whys := map[string]int{}    // 失败原因聚合（写进超时错误消息，用户可自查）
	type btMetaTry struct {
		ok  bool
		m   *btMeta
		why string
	}
	resCh := make(chan btMetaTry, 16)
	inflight := 0
	for meta == nil && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			t.setLock("stopped", "")
			return
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
		case <-time.After(2 * time.Second):
		}
	}
	if meta == nil {
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
			t.setLock("error", fmt.Sprintf("获取种子信息超时：未发现任何 peer%s——该资源可能无做种在线；若 DHT 节点为 0 则是本机网络限制 UDP / tracker 访问", dmsg))
		} else {
			t.setLock("error", fmt.Sprintf("获取种子信息超时：已尝试 peer %d%s%s 但无一成功——资源可能冷门或网络受限", seenN, wmsg, dmsg))
		}
		return
	}

	// ---- 文件布局进下载态 ----
	full, ferr := btPrepareFiles(t, meta)
	if ferr != nil {
		t.setLock("error", ferr.Error())
		return
	}
	defer full.btCloseAllPrep()
	t.mu.Lock()
	t.meta = meta
	t.have = make([]bool, meta.btNumPieces())
	t.doneN = 0
	t.written = 0
	t.Status = "downloading"
	t.Err = ""
	t.mu.Unlock()

	// ---- 阶段 2：worker 池 ----
	tech := map[int]bool{} // 已分配未完成片（跨 worker）
	var techMu sync.Mutex
	stall := 0
	total := meta.btNumPieces()
	for {
		if ctx.Err() != nil {
			t.setLock("stopped", "")
			return
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
			recent := false
			if last, ok := t.inflight[addr]; ok && time.Since(last) < 30*time.Second {
				recent = true
			} else {
				t.inflight[addr] = time.Now()
			}
			need := active < btMaxConns
			t.mu.Unlock()
			if recent || !need {
				continue
			}
			go runWorker(ctx, t, addr, &techMu, tech, full)
		case <-time.After(4 * time.Second):
			stall++
			t.mu.Lock()
			okTech := len(tech) == 0
			t.mu.Unlock()
			// 3 分钟无速度、无连接、无挂起片 → 停滞收场
			if stall > 45 && noSpeed && active == 0 && okTech {
				t.setLock("error", "长时间无可用 peer，下载停滞（资源冷门或网络受限）")
				return
			}
		}
	}
	t.setLock("done", "")
	if n := dhtN(); n != nil {
		go n.announceAll(t.hash)
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
	binary.BigEndian.PutUint64(req[56:64], 0)   // downloaded
	binary.BigEndian.PutUint64(req[64:72], 1)   // left
	binary.BigEndian.PutUint64(req[72:80], 0)   // uploaded
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
	if len(meta.Files) == 0 {
		base := btCleanName(meta.Name)
		if base == "" {
			return nil, fmt.Errorf("种子文件名异常")
		}
		path := dlMusUniquePath(t.Dir, base)
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
func runWorker(ctx context.Context, t *btTask, addr string, techMu *sync.Mutex, tech map[int]bool, full *btPrepared) {
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
	var peerBF []bool
	curPiece := -1
	curBuf := []byte{}
	got := 0                                // 该片已收字节数（判定整片收齐，避免 sent 重扫把已收块再请求一遍）
	sent := map[int]bool{}  // 该片已请求未回的块偏移
	recv := map[int]bool{}  // 该片已收到的块偏移
	holding := false
	defer func() { // worker 退出释放挂起片
		if holding && curPiece >= 0 {
			techMu.Lock()
			delete(tech, curPiece)
			techMu.Unlock()
		}
	}()
	pendAt := time.Now()
	idleRound := 0
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
				techMu.Lock()
				delete(tech, curPiece)
				techMu.Unlock()
				holding, curPiece, curBuf, sent, recv, got = false, -1, nil, map[int]bool{}, map[int]bool{}, 0
			}
			continue
		case 20: // 扩展消息（下载态不消费）
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
				techMu.Lock()
				delete(tech, idx)
				techMu.Unlock()
				holding, curPiece, curBuf, sent, recv, got = false, -1, nil, map[int]bool{}, map[int]bool{}, 0
			}
		}
		// 选片 / 派块（bitfield 到手才能干活）
		if peerBF == nil {
			continue
		}
		if curPiece < 0 {
			np, ok := btNextPiece(t, techMu, tech, meta, peerBF)
			if !ok {
				t.mu.Lock()
				alldone := t.doneN >= meta.btNumPieces()
				t.mu.Unlock()
				if alldone {
					return
				}
				idleRound++
				if idleRound > 60 { // 这条 peer 没有我们缺的块，退场让位
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
		if len(sent) >= btPipelineBlocks {
			if time.Since(pendAt) > btStalePiece { // 全挂起但长期无返回：重置该片
				techMu.Lock()
				delete(tech, curPiece)
				techMu.Unlock()
				holding, curPiece, curBuf, sent, recv, got = false, -1, nil, map[int]bool{}, map[int]bool{}, 0
			}
		} else {
			for off := 0; off < len(curBuf) && len(sent) < btPipelineBlocks; off = nextBlockOff(curBuf, off) {
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

// ---------- 对外（dlapi 调） ----------

// btCancel 取消/停止任务
func btCancel(id string) {
	btInit()
	btStore.mu.Lock()
	t := btStore.tasks[id]
	btStore.mu.Unlock()
	if t != nil && t.cancel != nil {
		t.setLock("stopped", "")
		t.cancel()
	}
}

// btRemove 移除终态任务
func btRemove(id string) {
	btInit()
	btStore.mu.Lock()
	t, ok := btStore.tasks[id]
	if ok && t.Status != "pending" && t.Status != "fetching" && t.Status != "downloading" {
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
			v.Peers = t.peersActive
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
			Title:     strings.TrimSpace(t.Artist + " - " + t.Song),
			Status:    status,
			Done:      t.Bytes, Total: t.Totals,
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
	if strings.TrimSpace(dir) == "" {
		dir = dlDefaultDir("video")
	}
	return btCreate(magnet, dir, title)
}
