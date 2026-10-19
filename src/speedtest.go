package main

// speedtest.go —— 网络测速
//
// 设计要点：
//   1. 不开新端口：端点全挂主 mux（/api/speedtest/*），经 fnOS 网关/隧道可达。
//   2. 大流量不出网关：下载/上传在「NAS 后端 ↔ 外网测速节点」之间进行，
//      浏览器只经网关回传结果 JSON（几 KB），不占中转带宽。
//   3. 节点测活与竞速：并发 TCP 握手探测延迟，自动选最快节点；节点列表含
//      成都联通（用户指定）、北京联通、天津电信、中科大（教育网兜底）。
//   4. 阶段化进度：前端轮询 /api/speedtest/status 拿百分比与中间结果，
//      依次展示 延迟 → 下载 → 上传 → 完成。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ---------- 节点定义（bim-core 格式：GET /download  POST /upload） ----------

type stNode struct {
	ID    string `json:"id"`
	Name  string `json:"name"`  // 展示名
	Group string `json:"group"` // 运营商/地区
	Host  string `json:"-"`     // host:port，用于 TCP 探活
	Down  string `json:"-"`     // 下载测速 URL（可带 ?r= 随机参数防缓存）
	Up    string `json:"-"`     // 上传端点 URL
}

// 节点列表：bim-core 测速源（i-abc/speedtest 项目维护的实测集合）。
// 成都联通为用户指定节点，排第一参与竞速。
var stNodes = []stNode{
	{ID: "cd-unicom", Name: "四川成都联通", Group: "联通", Host: "speedtest.cnscn96655.com:8080", Down: "http://speedtest.cnscn96655.com:8080/download", Up: "http://speedtest.cnscn96655.com:8080/upload"},
	{ID: "bj-unicom", Name: "北京联通", Group: "联通", Host: "beijing.unicomtest.com:8080", Down: "https://beijing.unicomtest.com:8080/download", Up: "https://beijing.unicomtest.com:8080/upload"},
	{ID: "tj-telecom", Name: "天津电信", Group: "电信", Host: "tjrate.tjtele.com:8080", Down: "http://tjrate.tjtele.com:8080/download", Up: "http://tjrate.tjtele.com:8080/upload"},
	{ID: "ustc", Name: "中科大（教育网）", Group: "教育网", Host: "test.ustc.edu.cn:443", Down: "https://test.ustc.edu.cn/backend/garbage.php", Up: "https://test.ustc.edu.cn/backend/empty.php"},
}

// ---------- 运行状态 ----------

type stHistoryItem struct {
	At       string  `json:"at"`
	Server   string  `json:"server"`
	PingMs   float64 `json:"ping"`
	JitterMs float64 `json:"jitter"`
	LossPct  float64 `json:"loss"`
	DownMbps float64 `json:"down"`
	UpMbps   float64 `json:"up"`
}

type stState struct {
	mu       sync.Mutex
	running  bool
	cancel   context.CancelFunc
	phase    string  // idle | ping | down | up | done | error
	progress int     // 当前阶段进度 0-100
	msg      string  // 附加信息（当前节点等）
	server   string  // 选定节点展示名
	pingMs   float64
	jitterMs float64
	lossPct  float64
	downMbps float64
	upMbps   float64
	downMB   float64 // 下载阶段累计流量 MB
	upMB     float64 // 上传阶段累计流量 MB
	errMsg   string
	history  []stHistoryItem
	histPath string // 持久化文件（initSpeedtest 赋值）
}

var st = &stState{phase: "idle", history: []stHistoryItem{}}

func initSpeedtest(dataDir string) {
	st.histPath = filepath.Join(dataDir, "speedtest_history.json")
	if b, err := os.ReadFile(st.histPath); err == nil {
		var h []stHistoryItem
		if json.Unmarshal(b, &h) == nil && h != nil {
			st.history = h
		}
	}
}

func (s *stState) snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{
		"running":  s.running,
		"phase":    s.phase,
		"progress": s.progress,
		"msg":      s.msg,
		"server":   s.server,
		"ping":     s.pingMs,
		"jitter":   s.jitterMs,
		"loss":     s.lossPct,
		"down":     s.downMbps,
		"up":       s.upMbps,
		"down_mb":  s.downMB,
		"up_mb":    s.upMB,
		"error":    s.errMsg,
		"history":  s.history,
	}
}

// ---------- HTTP handlers ----------

// GET /api/speedtest/status — 状态轮询 + 历史记录
func handleSpeedtestStatus(w http.ResponseWriter, r *http.Request) {
	ok(w, st.snapshot())
}

// POST /api/speedtest/start — 启动一轮测速（运行中则直接返回当前状态）
func handleSpeedtestStart(w http.ResponseWriter, r *http.Request) {
	st.mu.Lock()
	if st.running {
		st.mu.Unlock()
		ok(w, st.snapshot())
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	st.running = true
	st.cancel = cancel
	st.phase = "ping"
	st.progress = 0
	st.msg = "探测测速节点…"
	st.server = ""
	st.pingMs, st.jitterMs, st.lossPct = 0, 0, 0
	st.downMbps, st.upMbps, st.downMB, st.upMB = 0, 0, 0, 0
	st.errMsg = ""
	st.mu.Unlock()

	go st.run(ctx)
	ok(w, st.snapshot())
}

// POST /api/speedtest/stop — 中止测速
func handleSpeedtestStop(w http.ResponseWriter, r *http.Request) {
	st.mu.Lock()
	if st.running && st.cancel != nil {
		st.cancel()
	}
	st.mu.Unlock()
	ok(w, st.snapshot())
}

// POST /api/speedtest/clear — 清空历史记录
func handleSpeedtestClear(w http.ResponseWriter, r *http.Request) {
	st.mu.Lock()
	st.history = []stHistoryItem{}
	st.saveHistLocked()
	st.mu.Unlock()
	ok(w, st.snapshot())
}

// ---------- 测速主流程 ----------

func (s *stState) setPhase(phase string, progress int, msg string) {
	s.mu.Lock()
	s.phase, s.progress, s.msg = phase, progress, msg
	s.mu.Unlock()
}

func (s *stState) fail(ctx context.Context, msg string) {
	s.mu.Lock()
	s.running = false
	s.phase = "error"
	s.errMsg = msg
	s.mu.Unlock()
	_ = ctx
}

func (s *stState) run(ctx context.Context) {
	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()

	// ---- 阶段 1：并发探活，选延迟最低的节点 ----
	type probe struct {
		node stNode
		ms   float64
	}
	probes := make([]probe, 0, len(stNodes))
	var pmu sync.Mutex
	var wg sync.WaitGroup
	for _, n := range stNodes {
		wg.Add(1)
		go func(n stNode) {
			defer wg.Done()
			ms, lost := stProbeNode(ctx, n.Host)
			if ms > 0 {
				pmu.Lock()
				probes = append(probes, probe{n, ms})
				pmu.Unlock()
			}
			if lost {
				// 全部丢包：不参与选择
			}
		}(n)
	}
	// 探活整体限时 5s
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	case <-ctx.Done():
		s.fail(ctx, "已中止")
		return
	}
	if len(probes) == 0 {
		s.fail(ctx, "所有测速节点均不可达，请检查 NAS 外网连接")
		return
	}
	sort.Slice(probes, func(i, j int) bool { return probes[i].ms < probes[j].ms })
	best := probes[0]

	// 延迟/抖动/丢包：对选定节点连续 TCP 握手 12 次
	s.setPhase("ping", 0, "测试延迟（"+best.node.Name+"）")
	var lat []float64
	lostCnt := 0
	const rounds = 12
	for i := 0; i < rounds; i++ {
		if ctx.Err() != nil {
			s.fail(ctx, "已中止")
			return
		}
		t0 := time.Now()
		c, err := net.DialTimeout("tcp", best.node.Host, 2*time.Second)
		if err != nil {
			lostCnt++
		} else {
			c.Close()
			lat = append(lat, float64(time.Since(t0).Microseconds())/1000.0)
		}
		s.mu.Lock()
		s.progress = (i + 1) * 100 / rounds
		s.mu.Unlock()
		time.Sleep(60 * time.Millisecond)
	}
	var pingMs, jitterMs float64
	if len(lat) > 0 {
		var sum float64
		for _, v := range lat {
			sum += v
		}
		pingMs = sum / float64(len(lat))
		// 抖动：相邻两次延迟差绝对值的平均
		var jd float64
		for i := 1; i < len(lat); i++ {
			jd += absf(lat[i] - lat[i-1])
		}
		jitterMs = jd / float64(len(lat)-1)
	}
	lossPct := float64(lostCnt) * 100 / float64(rounds)
	s.mu.Lock()
	s.server = best.node.Name
	s.pingMs, s.jitterMs, s.lossPct = r1(pingMs), r1(jitterMs), r1(lossPct)
	s.mu.Unlock()

	client := &http.Client{
		Timeout:   0, // 阶段内部自行限流
		Transport: &http.Transport{DisableCompression: true, MaxIdleConnsPerHost: 6},
	}

	// ---- 阶段 2：下载（多连接 8s，取后 60% 平均速率） ----
	s.setPhase("down", 0, "下载测速（"+best.node.Name+"）")
	downMbps, downMB, derr := stDownload(ctx, client, best.node.Down)
	if derr != nil && ctx.Err() == nil {
		s.fail(ctx, "下载测速失败："+derr.Error())
		return
	}
	if ctx.Err() != nil {
		s.fail(ctx, "已中止")
		return
	}
	s.mu.Lock()
	s.downMbps, s.downMB = r1(downMbps), r1(downMB)
	s.mu.Unlock()

	// ---- 阶段 3：上传（多连接 8s） ----
	s.setPhase("up", 0, "上传测速（"+best.node.Name+"）")
	upMbps, upMB, uerr := stUpload(ctx, client, best.node.Up)
	if uerr != nil && ctx.Err() == nil {
		s.fail(ctx, "上传测速失败："+uerr.Error())
		return
	}
	if ctx.Err() != nil {
		s.fail(ctx, "已中止")
		return
	}
	s.mu.Lock()
	s.upMbps, s.upMB = r1(upMbps), r1(upMB)
	s.mu.Unlock()

	// ---- 完成：写历史 ----
	s.mu.Lock()
	s.phase = "done"
	s.progress = 100
	s.msg = "测速完成"
	item := stHistoryItem{
		At:       time.Now().Format("2006-01-02 15:04:05"),
		Server:   s.server,
		PingMs:   s.pingMs,
		JitterMs: s.jitterMs,
		LossPct:  s.lossPct,
		DownMbps: s.downMbps,
		UpMbps:   s.upMbps,
	}
	s.history = append([]stHistoryItem{item}, s.history...)
	if len(s.history) > 30 {
		s.history = s.history[:30]
	}
	s.saveHistLocked()
	s.mu.Unlock()
}

func (s *stState) saveHistLocked() {
	if s.histPath == "" {
		return
	}
	b, err := json.Marshal(s.history)
	if err == nil {
		_ = os.WriteFile(s.histPath, b, 0o644)
	}
}

// ---------- 探活与速率测算 ----------

// TCP 握手探活：返回平均延迟 ms（不可达返回 0）
func stProbeNode(ctx context.Context, host string) (float64, bool) {
	var total time.Duration
	n := 3
	fail := 0
	for i := 0; i < n; i++ {
		t0 := time.Now()
		c, err := net.DialTimeout("tcp", host, 1500*time.Millisecond)
		if err != nil {
			fail++
			continue
		}
		c.Close()
		total += time.Since(t0)
	}
	if fail == n {
		return 0, true
	}
	return float64(total.Microseconds()) / 1000.0 / float64(n-fail), false
}

// 下载：8 连接并发拉流 8 秒，丢弃前 2 秒（TCP 爬坡），后 6 秒平均速率
func stDownload(ctx context.Context, client *http.Client, url string) (mbps, totalMB float64, err error) {
	phaseCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	var mu sync.Mutex
	var bytesSum int64
	var lastErr error
	var wg sync.WaitGroup
	const conns = 8
	for i := 0; i < conns; i++ {
		wg.Add(1)
		go func(seq int) {
			defer wg.Done()
			req, e := http.NewRequestWithContext(phaseCtx, "GET", url+"?r="+fmt.Sprintf("%d%d", time.Now().UnixNano(), seq), nil)
			if e != nil {
				return
			}
			req.Header.Set("Cache-Control", "no-cache")
			resp, e := client.Do(req)
			if e != nil {
				mu.Lock()
				lastErr = e
				mu.Unlock()
				return
			}
			defer resp.Body.Close()
			buf := make([]byte, 128*1024)
			for {
				nn, e := resp.Body.Read(buf)
				if nn > 0 {
					mu.Lock()
					bytesSum += int64(nn)
					mu.Unlock()
				}
				if e != nil {
					break // EOF / 超时 / 取消都是正常结束
				}
			}
		}(i)
	}
	// 进度 + 总量采样：500ms 一次，共约 16 个采样点
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	waitDone := make(chan struct{})
	go func() { wg.Wait(); close(waitDone) }()
	start := time.Now()
	samples := map[int64]int64{} // 秒偏移 → 累计字节
	for {
		select {
		case <-waitDone:
			goto done
		case <-phaseCtx.Done():
			goto done
		case <-tick.C:
			mu.Lock()
			s := bytesSum
			mu.Unlock()
			off := int64(time.Since(start).Seconds())
			samples[off] = s
			// 进度：以 8 秒为满
			p := int(time.Since(start).Seconds() * 100 / 8)
			if p > 99 {
				p = 99
			}
			st.mu.Lock()
			st.progress = p
			st.downMB = r1(float64(s) / 1e6)
			st.mu.Unlock()
		}
	}
done:
	if bytesSum == 0 && lastErr != nil {
		return 0, 0, lastErr
	}
	elapsed := time.Since(start).Seconds()
	if elapsed < 1 {
		elapsed = 1
	}
	totalMB = float64(bytesSum) / 1e6
	// 稳态速率：若采样覆盖 >=3 秒，取 2 秒后窗口的平均；否则取全程
	useFrom := int64(2)
	base := int64(0)
	if v, has := samples[useFrom]; has {
		base = v
		if span := time.Since(start).Seconds() - float64(useFrom); span >= 2 {
			mbps = float64(bytesSum-base) * 8 / span / 1e6
			return mbps, totalMB, nil
		}
	}
	mbps = float64(bytesSum) * 8 / elapsed / 1e6
	return mbps, totalMB, nil
}

// 上传：8 连接并发 POST 随机块 8 秒
func stUpload(ctx context.Context, client *http.Client, url string) (mbps, totalMB float64, err error) {
	phaseCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	var mu sync.Mutex
	var bytesSum int64
	var lastErr error
	var wg sync.WaitGroup
	const conns = 8
	const chunk = 256 * 1024 // 每块 256KB
	block := make([]byte, chunk)
	for i := range block {
		block[i] = byte(i % 251)
	}
	for i := 0; i < conns; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if phaseCtx.Err() != nil {
					return
				}
				req, e := http.NewRequestWithContext(phaseCtx, "POST", url, nil)
				if e != nil {
					mu.Lock()
					lastErr = e
					mu.Unlock()
					return
				}
				req.ContentLength = int64(chunk)
				req.Header.Set("Content-Type", "application/octet-stream")
				// 包一层计数 reader，统计真实发送字节数
				var sent int64
				req.Body = io.NopCloser(&countReader{r: newLoopReader(block), n: &sent, limit: int64(chunk)})
				resp, e := client.Do(req)
				if e != nil {
					mu.Lock()
					lastErr = e
					bytesSum += sent
					mu.Unlock()
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				mu.Lock()
				bytesSum += sent
				mu.Unlock()
			}
		}()
	}
	waitDone := make(chan struct{})
	go func() { wg.Wait(); close(waitDone) }()
	start := time.Now()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
loop:
	for {
		select {
		case <-waitDone:
			break loop
		case <-phaseCtx.Done():
			break loop
		case <-tick.C:
			mu.Lock()
			s := bytesSum
			mu.Unlock()
			p := int(time.Since(start).Seconds() * 100 / 8)
			if p > 99 {
				p = 99
			}
			st.mu.Lock()
			st.progress = p
			st.upMB = r1(float64(s) / 1e6)
			st.mu.Unlock()
		}
	}
	mu.Lock()
	bs := bytesSum
	le := lastErr
	mu.Unlock()
	if bs == 0 && le != nil {
		return 0, 0, le
	}
	elapsed := time.Since(start).Seconds()
	if elapsed < 1 {
		elapsed = 1
	}
	totalMB = float64(bs) / 1e6
	mbps = float64(bs) * 8 / elapsed / 1e6
	return mbps, totalMB, nil
}

// loopReader：无限循环输出 block 内容；countReader 包装计数，达到 limit 报 EOF
type loopReader struct {
	b   []byte
	pos int
}

func newLoopReader(b []byte) *loopReader { return &loopReader{b: b} }

func (l *loopReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	n := 0
	for n < len(p) {
		c := copy(p[n:], l.b[l.pos:])
		l.pos = (l.pos + c) % len(l.b)
		n += c
	}
	return n, nil
}

type countReader struct {
	r     *loopReader
	n     *int64
	limit int64
}

func (c *countReader) Read(p []byte) (int, error) {
	if *c.n >= c.limit {
		return 0, io.EOF
	}
	nn, err := c.r.Read(p)
	*c.n += int64(nn)
	return nn, err
}

// ---------- 工具 ----------

func absf(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func r1(v float64) float64 {
	return float64(int(v*10+0.5)) / 10
}
