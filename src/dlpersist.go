package main

// 下载任务持久化（v1.3.52）
//
// 背景：BT 引擎与音乐下载的任务表都是纯内存结构（btStore / dlMusStore），进程一退
// （用户关闭应用、fpk 升级换包重启、崩溃重启）整张列表就空掉——用户升级完回来发现
// 「下载任务全丢了」。文件其实还在盘上，丢的只是任务记录与分片进度。
//
// 方案：把两个任务表周期性快照到 ${XBOX_DATA}/dl-tasks.json（临时文件 + rename 原子写），
// 启动时读回并自动续传。${XBOX_DATA} 是 fnOS 应用持久数据卷（TRIM_PKGVAR），升级保留。
//   - BT：infohash + tracker 列表 + 分片位图（have）落盘。位图保证重启后不回退进度；
//     配合已有的 torrent-meta 元数据缓存，续传时不必重抓元数据、不重建文件（reuseFiles
//     让 btPrepareFiles 沿用原文件名，而不是造一个「xxx (1)」副本）。
//   - 音乐直链：保留 .part 与已下载字节，续传走 HTTP Range。
//   - 只有 done/error/stopped/cancel 这类终态不重新拉起，仅保留历史展示。
//
// 红线：
//   - 持久化失败绝不影响下载本身（读写错误一律静默忽略）；
//   - 恢复时目录不可用、infohash 非法的记录直接跳过，不许凭空造出一个会写错位置的任务；
//   - 快照写盘只用「同目录临时文件 + rename」，中途被杀不会留下半截 JSON。

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	dlPersistVer = 1
	dlPersistMax = 8 << 20 // 快照上限，超出拒写（防止异常膨胀拖死磁盘）
	dlPersistRecs = 200    // 单次快照最多保留的任务条数（超出丢最老）
)

// dlPersistTick 周期快照间隔（运行时可变：测试把它拉长避免后台写盘干扰临时目录）
var dlPersistTick = 8 * time.Second

// dlMetaLoad 元数据缓存读取（断点恢复用；测试可替换为假元数据，避免真去抓种子）
var dlMetaLoad = btMetaCacheGet

// dlPersistRec 单条任务记录（BT 与音乐直链共用，按 kind 区分）
type dlPersistRec struct {
	Kind      string    `json:"kind"` // bt / music
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Dir       string    `json:"dir"`
	Status    string    `json:"status"`
	Err       string    `json:"err,omitempty"`
	CreatedAt time.Time `json:"createdAt"`

	// BT
	Hash     string   `json:"hash,omitempty"`
	Trackers []string `json:"trackers,omitempty"`
	Written  int64    `json:"written,omitempty"`
	Total    int64    `json:"total,omitempty"`
	HaveB64  string   `json:"have,omitempty"` // 分片位图（bit i 对应第 i 片，MSB 在前）
	Paused   bool     `json:"paused,omitempty"`

	// 音乐直链
	Source string `json:"source,omitempty"`
	SongID int64  `json:"songID,omitempty"`
	Rid    string `json:"rid,omitempty"`
	Song   string `json:"song,omitempty"`
	Artist string `json:"artist,omitempty"`
	Path   string `json:"path,omitempty"`
	Bytes  int64  `json:"bytes,omitempty"`
	Totals int64  `json:"totals,omitempty"`

	// 短剧 m3u8（v1.3.58）
	Eps    []DlM3u8Ep `json:"eps,omitempty"` // 全部分集（含 m3u8 直链）
	DoneEp int        `json:"doneEp,omitempty"`
}

// dlPersistDoc 快照文件结构
type dlPersistDoc struct {
	V       int            `json:"v"`
	SavedAt time.Time      `json:"savedAt"`
	Tasks   []dlPersistRec `json:"tasks"`
}

// dlRestorePeers 测试注入：非 nil 时恢复出来的 BT 任务只直连这些地址（跳过 tracker/DHT，
// 与 btCreateEx 的 testPeers 同一机制）。生产恒为 nil。
var dlRestorePeers []string

var dlPersistState struct {
	mu      sync.Mutex
	last    []byte
	started bool
}

// dlPendingRestore 恢复时「目录暂时不可用」的记录（外挂存储/网络卷晚于应用挂载）。
// 这类记录不能丢：留在内存里每轮快照前重试，期间照原样写回快照文件。
var dlPendingRestore struct {
	mu   sync.Mutex
	recs []dlPersistRec
}

// dlPendingPush 记下一条待重试记录（同 id 去重）
func dlPendingPush(r dlPersistRec) {
	dlPendingRestore.mu.Lock()
	defer dlPendingRestore.mu.Unlock()
	for _, ex := range dlPendingRestore.recs {
		if ex.Kind == r.Kind && ex.ID == r.ID {
			return
		}
	}
	dlPendingRestore.recs = append(dlPendingRestore.recs, r)
}

// dlPendingClear 清空待重试记录（用户点「清空全部」时同步丢弃，否则下次快照又写回去）
func dlPendingClear() {
	dlPendingRestore.mu.Lock()
	dlPendingRestore.recs = nil
	dlPendingRestore.mu.Unlock()
}

// dlPendingRetry 再试一次所有待恢复记录（存储卷挂上来后自动补回任务）
func dlPendingRetry() {
	dlPendingRestore.mu.Lock()
	recs := dlPendingRestore.recs
	dlPendingRestore.recs = nil
	dlPendingRestore.mu.Unlock()
	if len(recs) == 0 {
		return
	}
	var keep []dlPersistRec
	for _, r := range recs {
		retry := false
		switch r.Kind {
		case "music":
			retry = dlRestoreMusic(r)
		case "m3u8":
			retry = dlRestoreM3u8(r)
		default:
			retry = dlRestoreBT(r)
		}
		if retry {
			keep = append(keep, r)
		}
	}
	dlPendingRestore.mu.Lock()
	dlPendingRestore.recs = append(keep, dlPendingRestore.recs...)
	dlPendingRestore.mu.Unlock()
}

// dlPersistPath 快照文件路径
func dlPersistPath() string {
	base := os.Getenv("XBOX_DATA")
	if base == "" {
		base = "."
	}
	return filepath.Join(base, "dl-tasks.json")
}

// dlPersistStart 拉起后台周期快照（恢复完成后调用，确保不会先用空表覆盖磁盘记录）
func dlPersistStart() {
	dlPersistState.mu.Lock()
	if dlPersistState.started {
		dlPersistState.mu.Unlock()
		return
	}
	dlPersistState.started = true
	dlPersistState.mu.Unlock()
	go func() {
		defer func() { recover() }()
		for {
			time.Sleep(dlPersistTick)
			dlPersistWrite()
		}
	}()
}

// dlPersistHandleExit 收到 SIGTERM/SIGINT 时先落盘再退出。
// fnOS 停止/升级应用走 kill -TERM（10s 后才 KILL），在此把最新任务状态写实一次，
// 关闭/升级后重启能看到「刚刚」的进度，而不是最多 8s 前的。
func dlPersistHandleExit() {
	go func() {
		defer func() { recover() }()
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
		<-ch
		dlPersistWrite()
		os.Exit(0)
	}()
}

// dlPersistTouch 状态变更后立即落盘（异步，绝不阻塞请求/引擎）
func dlPersistTouch() {
	go func() {
		defer func() { recover() }()
		dlPersistWrite()
	}()
}

// dlPersistWrite 立即快照落盘；内容与上次一致则跳过写盘（省 flash 写入）
func dlPersistWrite() {
	dlPendingRetry() // 先补上「目录暂不可用」的记录，再统一快照
	doc := dlPersistDoc{V: dlPersistVer, Tasks: dlPersistSnapshot()}
	b, err := json.Marshal(doc)
	if err != nil || len(b) > dlPersistMax {
		return
	}
	dlPersistState.mu.Lock()
	if bytes.Equal(b, dlPersistState.last) {
		dlPersistState.mu.Unlock()
		return
	}
	dlPersistState.mu.Unlock()

	p := dlPersistPath()
	tmp := p + ".tmp"
	if werr := os.WriteFile(tmp, b, 0o644); werr != nil {
		return
	}
	if rerr := os.Rename(tmp, p); rerr != nil {
		os.Remove(tmp)
		return
	}
	dlPersistState.mu.Lock()
	dlPersistState.last = b
	dlPersistState.mu.Unlock()
}

// dlPersistSnapshot 采集当前全部任务（BT + 音乐直链）
func dlPersistSnapshot() []dlPersistRec {
	btInit()
	out := make([]dlPersistRec, 0, 8)

	btStore.mu.Lock()
	tasks := make([]*btTask, 0, len(btStore.order))
	for _, id := range btStore.order {
		if t := btStore.tasks[id]; t != nil {
			tasks = append(tasks, t)
		}
	}
	btStore.mu.Unlock()

	for _, t := range tasks {
		t.mu.Lock()
		r := dlPersistRec{
			Kind: "bt", ID: t.ID, Title: t.Title, Dir: t.Dir,
			Status: t.Status, Err: t.Err, CreatedAt: t.CreatedAt,
			Hash:     btHashHex(t.hash),
			Trackers: append([]string(nil), t.trackers...),
			Written:  t.written, Paused: t.paused,
		}
		if t.meta != nil {
			r.Total = t.meta.btTotalSize()
			if len(t.have) > 0 {
				// 有位图就以位图为唯一进度来源（恢复时按位图重算字节数），
				// 不再单独写 written —— 否则每 8s 都会因字节数变化重写一次磁盘
				r.HaveB64 = dlBitSetB64(t.have)
			}
		}
		if r.HaveB64 == "" {
			r.Written = t.written
		}
		t.mu.Unlock()
		out = append(out, r)
	}

	for _, t := range dlMusTaskList() {
		out = append(out, dlPersistRec{
			Kind: "music", ID: t.ID, Title: strings.TrimSpace(t.Artist + " - " + t.Song),
			Dir: t.Dir, Status: t.Status, Err: t.Err, CreatedAt: t.At,
			Source: t.Source, SongID: t.songID, Rid: t.kwItem.Rid,
			Song: t.Song, Artist: t.Artist, Path: t.Path,
			Bytes: t.Bytes, Totals: t.Totals, Paused: t.Status == "paused",
		})
	}

	for _, t := range dlM3u8TaskList() {
		r := dlPersistRec{
			Kind: "m3u8", ID: t.ID, Title: t.Title,
			Dir: t.Dir, Status: t.Status, Err: t.Err, CreatedAt: t.CreatedAt,
			Eps: t.Eps, DoneEp: t.DoneEps, Paused: t.Status == "paused",
		}
		out = append(out, r)
	}

	// 超出上限丢最老的（BT 在前、音乐在后，按创建时间统一裁）；上限只为防异常膨胀
	out = append(out, dlPendingRecs()...)
	if len(out) > dlPersistRecs {
		sortRecsByNewest(out)
		out = out[:dlPersistRecs]
	}
	return out
}

// dlPendingRecs 待重试记录的快照副本（原样写回，避免误删）
func dlPendingRecs() []dlPersistRec {
	dlPendingRestore.mu.Lock()
	defer dlPendingRestore.mu.Unlock()
	return append([]dlPersistRec(nil), dlPendingRestore.recs...)
}

// sortRecsByNewest 任务按创建时间倒序（稳定排序避免同秒抖动）
func sortRecsByNewest(rs []dlPersistRec) {
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0 && rs[j].CreatedAt.After(rs[j-1].CreatedAt); j-- {
			rs[j], rs[j-1] = rs[j-1], rs[j]
		}
	}
}

// dlRestoreTasks 启动时读回快照并恢复任务；随后拉起周期快照。
// 任何读取/解析失败都当「没有历史任务」，不许拖住启动流程。
func dlRestoreTasks() {
	var doc dlPersistDoc
	if b, err := os.ReadFile(dlPersistPath()); err == nil && len(b) > 0 && len(b) <= dlPersistMax {
		if json.Unmarshal(b, &doc) != nil {
			doc.Tasks = nil
		}
	}
	if len(doc.Tasks) > 0 {
		btApplyTune(loadDlConfig()) // 恢复的任务按设置页的并发数排队
	}
	// 按创建时间正序恢复，任务列表顺序与重启前一致
	sortRecsOldestFirst(doc.Tasks)
	for _, r := range doc.Tasks {
		retry := false
		switch r.Kind {
		case "music":
			retry = dlRestoreMusic(r)
		case "m3u8":
			retry = dlRestoreM3u8(r)
		default:
			retry = dlRestoreBT(r)
		}
		if retry {
			dlPendingPush(r) // 目录暂不可用（外挂卷未挂载等）：留着稍后重试，别丢任务
		}
	}
	// 恢复完成后立刻落一次盘（把被判为 error/跳过 的记录归位），再开周期快照
	dlPersistWrite()
	dlPersistStart()
}

// sortRecsOldestFirst 恢复顺序：老的先入表（列表按时间倒序展示，顺序与退出前一致）
func sortRecsOldestFirst(rs []dlPersistRec) {
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0 && rs[j].CreatedAt.Before(rs[j-1].CreatedAt); j-- {
			rs[j], rs[j-1] = rs[j-1], rs[j]
		}
	}
}

// dlRestoreBT 恢复一个 BT 任务。
// 有元数据缓存 → 预开文件布局 + 还原位图（真断点续传）；
// 没有缓存 → 只登记任务，让引擎重抓元数据后从头校验（文件复用，不产生副本）。
// 返回 true 表示「目录暂时不可用、值得稍后重试」（外挂存储晚挂载），记录须保留。
func dlRestoreBT(r dlPersistRec) (retry bool) {
	dir := strings.TrimSpace(r.Dir)
	if dir == "" {
		return false
	}
	if _, safe := safeDownloadDir(dir); !safe {
		return true // 目录已删或存储卷未就绪：宁可不恢复，也不往错位置写
	}
	h, ok := dlHashFromHex(r.Hash)
	if !ok {
		return false
	}
	id := r.ID
	if id == "" || !strings.HasPrefix(id, "bt") {
		id = btNewTaskID(h)
	}

	btInit()
	btStore.mu.Lock()
	_, dup := btStore.tasks[id]
	btStore.mu.Unlock()
	if dup {
		return false
	}

	t := &btTask{
		ID: id, Title: r.Title, Dir: dir, Err: r.Err,
		CreatedAt: r.CreatedAt, hash: h,
		inflight:   map[string]time.Time{},
		trackers:   r.Trackers,
		reuseFiles: true, // 续传沿用原文件名（不要造「xxx (1)」）
	}
	if t.Title == "" {
		t.Title = btHashHex(h)[:8]
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}
	t.ctx, t.cancel = context.WithCancel(context.Background())
	if dlRestorePeers != nil {
		t.testPeers = dlRestorePeers
	}

	// 元数据缓存命中 → 还原分片位图与已下字节（缓存未命中就等引擎重抓）
	if meta := dlMetaLoad(h); meta != nil {
		t.meta = meta
		n := meta.btNumPieces()
		t.have = dlBitSetFromB64(r.HaveB64, n)
		t.doneN = dlBitSetCount(t.have)
		t.written = btHaveWritten(meta, t.have)
	}

	terminal := r.Status == "done" || r.Status == "error" || r.Status == "stopped"
	resume := false
	switch {
	case terminal:
		t.Status = r.Status
		if t.Status == "done" && t.meta != nil {
			t.written = t.meta.btTotalSize() // 已完成任务进度归一（位图可能缺尾部空片）
		}
	case r.Status == "paused" || r.Paused:
		// 暂停态：保留断点，不自动拉起（用户没让它下）
		t.paused = true
		t.Status = "paused"
	default:
		// pending/fetching/downloading（进程被杀时多半停在这几个态）→ 自动续传
		if t.meta != nil {
			t.Status = "downloading"
			resume = true
		} else {
			t.Status = "fetching"
			resume = true
		}
	}

	// 非终态先预开文件布局并保留在任务里：btRunOnce 只有在 prep==nil 时才会把
	// have/doneN/written 清零，预开后进度才能续上。
	if !terminal && t.meta != nil {
		if prep, perr := btPrepareFiles(t, t.meta); perr == nil {
			t.prep = prep
			// 位图必须与盘上数据对得上：文件被删/被截断时若照信位图，缺片永远不会再下，
			// 最终得到一个「进度 100% 但内容残缺」的坏文件。盘上字节不足就整片重下。
			if t.doneN > 0 && btDiskBytes(t, t.meta) < t.written {
				t.have = make([]bool, t.meta.btNumPieces())
				t.doneN, t.written = 0, 0
			}
		} else {
			t.Status, t.Err = "error", perr.Error()
			resume = false
		}
	}

	btStore.mu.Lock()
	btStore.tasks[id] = t
	btStore.order = append(btStore.order, id)
	btStore.mu.Unlock()

	if resume {
		btStartWork(t)
	}
	return false
}

// dlRestoreMusic 恢复一个音乐直链任务（running/pending 从 .part 断点续传）。
// 返回 true 表示目录暂时不可用、值得稍后重试。
func dlRestoreMusic(r dlPersistRec) (retry bool) {
	dlMusInit()
	id := r.ID
	if id == "" || strings.HasPrefix(id, "bt") {
		return false
	}
	if _, safe := safeDownloadDir(r.Dir); !safe {
		return true
	}
	path := strings.TrimSpace(r.Path)
	if path == "" {
		return false
	}
	dlMusStore.mu.Lock()
	_, dup := dlMusStore.tasks[id]
	dlMusStore.mu.Unlock()
	if dup {
		return false
	}

	src := r.Source
	if src == "" {
		src = "netease"
	}
	at := r.CreatedAt
	if at.IsZero() {
		at = time.Now()
	}
	st := &DlMusicTask{
		ID: id, Song: r.Song, Artist: r.Artist, Source: src, Path: path,
		Status: r.Status, Bytes: r.Bytes, Totals: r.Totals, Err: r.Err,
		Dir: r.Dir, At: at, songID: r.SongID,
		kwItem: DlKwItem{Rid: r.Rid, Name: r.Song, Artist: r.Artist},
	}
	dlMusStore.mu.Lock()
	dlMusStore.tasks[id] = st
	dlMusStore.mu.Unlock()

	switch r.Status {
	case "paused":
		st.paused = true
	case "ok", "error", "cancel":
		// 终态：只保留记录，不重下
	default:
		// dlMusLaunch 见到 running 会直接 return（判定「已在跑」），先降到 pending
		dlMusStore.mu.Lock()
		st.Status = "pending"
		dlMusStore.mu.Unlock()
		dlMusLaunch(st)
	}
	return false
}

// dlRestoreM3u8 恢复一个短剧 m3u8 任务（v1.3.58）。
// 已存在的集不重下（dlM3u8FirstPending 按盘上 .ts 判断），只补未完成集。
// 返回 true 表示目录暂时不可用、值得稍后重试。
func dlRestoreM3u8(r dlPersistRec) (retry bool) {
	dlM3u8Init()
	id := r.ID
	if id == "" || !strings.HasPrefix(id, "m3u8") {
		return false
	}
	if _, safe := safeDownloadDir(r.Dir); !safe {
		return true // 目录不存在/存储卷未就绪：宁可不恢复
	}
	if len(r.Eps) == 0 {
		return false
	}
	dlM3u8Store.mu.Lock()
	_, dup := dlM3u8Store.tasks[id]
	dlM3u8Store.mu.Unlock()
	if dup {
		return false
	}
	at := r.CreatedAt
	if at.IsZero() {
		at = time.Now()
	}
	t := &DlM3u8Task{
		ID: id, Title: r.Title, Dir: r.Dir,
		Status: r.Status, Err: r.Err, CreatedAt: at, At: at,
		Eps: r.Eps, DoneEps: r.DoneEp, CurEp: -1, paused: r.Paused,
	}
	// 视盘上已有文件重算已完成集数（跨集 .ts 已存在的直接跳过）
	if t.DoneEps == 0 {
		for i, ep := range t.Eps {
			if _, err := os.Stat(filepath.Join(t.Dir, dlM3u8EpName(ep, i))); err == nil {
				t.DoneEps = i + 1
			} else {
				break
			}
		}
	}
	dlM3u8Store.mu.Lock()
	dlM3u8Store.tasks[id] = t
	dlM3u8Store.mu.Unlock()

	switch r.Status {
	case "paused":
		t.paused = true
		t.Status = "paused"
	case "done", "error", "cancel":
		// 终态：只保留记录，不自动拉起
	default:
		// running/pending（进程被杀多半停在这）→ 自动续传
		dlM3u8Launch(t)
	}
	return false
}

// ---------- 分片位图 / 哈希工具 ----------

// dlBitSetB64 位图 → base64（bit i = have[i]，MSB 在前，与 BT bitfield 同序）
func dlBitSetB64(have []bool) string {
	if len(have) == 0 {
		return ""
	}
	b := make([]byte, (len(have)+7)/8)
	for i, v := range have {
		if v {
			b[i>>3] |= 1 << uint(7-i&7)
		}
	}
	return base64.StdEncoding.EncodeToString(b)
}

// dlBitSetFromB64 base64 → 位图（长度固定 n；数据缺失/损坏按全缺处理）
func dlBitSetFromB64(s string, n int) []bool {
	have := make([]bool, n)
	if s == "" || n <= 0 {
		return have
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return have
	}
	for i := 0; i < n && i>>3 < len(b); i++ {
		have[i] = b[i>>3]&(1<<uint(7-i&7)) != 0
	}
	return have
}

// dlBitSetCount 已置位数量
func dlBitSetCount(have []bool) int {
	n := 0
	for _, v := range have {
		if v {
			n++
		}
	}
	return n
}

// btHaveWritten 按位图算已落盘字节（末片按总长余数）
func btHaveWritten(meta *btMeta, have []bool) int64 {
	if meta == nil || len(have) == 0 {
		return 0
	}
	total := meta.btTotalSize()
	n := meta.btNumPieces()
	var w int64
	for i, ok := range have {
		if !ok {
			continue
		}
		if i == n-1 {
			last := total - int64(i)*meta.PieceLen
			if last > 0 {
				w += last
			}
			continue
		}
		w += meta.PieceLen
	}
	if w > total {
		w = total
	}
	return w
}

// btDiskBytes 任务产物在盘上的实际字节数（校验恢复出来的分片位图是否可信）
func btDiskBytes(t *btTask, meta *btMeta) int64 {
	var sum int64
	for _, p := range btTaskFiles(t) {
		fi, err := os.Stat(p)
		if err != nil || fi.IsDir() {
			continue
		}
		sum += fi.Size()
	}
	return sum
}

// dlHashFromHex 40 位 hex → infohash
func dlHashFromHex(s string) ([20]byte, bool) {
	var h [20]byte
	s = strings.TrimSpace(strings.ToLower(s))
	if len(s) != 40 {
		return h, false
	}
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 20 {
		return h, false
	}
	copy(h[:], raw)
	return h, true
}
