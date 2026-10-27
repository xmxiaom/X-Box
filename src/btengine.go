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
	"crypto/sha1"
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
	btMetaTryTimeout = 50 * time.Second
	btMaxTorrentSz   = int64(4) << 30
	btBlockLen       = 16 << 10 // 分块 16KB
	btPipelineBlocks = 8        // 单连接挂起块数
	btStalePiece     = 40 * time.Second
	btPeersChCap     = 256
)

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
		for _, tr := range trackers {
			go btTrackerLoop(ctx, tr, t.hash, feed)
		}
		if n := dhtN(); n != nil {
			go n.findPeers(ctx, t.hash, feed)
		}
	}

	// ---- 阶段 1：元数据（先到先得 + infohash 核对） ----
	var meta *btMeta
	deadline := time.Now().Add(btMetaTryTimeout)
	tried := map[string]bool{}
	for meta == nil && time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			t.setLock("stopped", "")
			return
		case a := <-peerCh:
			addr := a.String()
			if tried[addr] {
				continue
			}
			tried[addr] = true
			m, err := btFetchMetaFromPeer(ctx, addr, t.hash)
			if err != nil || m == nil {
				continue
			}
			meta = m
		case <-time.After(6 * time.Second):
		}
	}
	if meta == nil {
		t.setLock("error", "未能获取种子元数据（做种不在线或网络受限）")
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

// btTrackerLoop 中心 tracker 周期 announce（http/https；udp tracker 不实现）
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
			CreatedAt: t.CreatedAt, Dir: t.Dir, Peers: t.peersActive,
			Speed: t.speed, Done: t.written,
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
