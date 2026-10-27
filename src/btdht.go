package main

// 内置 BT 引擎 · DHT 层：KRPC(bencode over UDP) Kademlia 的最小子集。
//
// 覆盖能力（够用即可，不做海量路由表）：
//   - bootstrap：三个公共引导节点（UDP 6881）
//   - get_peers：向「最近节点池」扩散，把返回的 peers 值喂给任务
//   - announce_peer：任务下载完成后做种回报（带 get_peers 攒下的 token）
//   - 收到查询一律礼貌回复，顺带把远端节点收进池子
//
// 规格取舍：
//   - 路由表退化为「容量 2048 的尽力而为节点池」，不建 160 位树桶；冷门资源慢些，
//     实现少 300 行——视频下载的主要 peer 来源是中心 tracker（.torrent 磁力自带的 tr=），
//     DHT 是无 tracker 磁力的兜底。
//   - 全部 UDP 包限长 2KB 读，防畸形包打爆缓冲。

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	dhtMaxNodes     = 2048
	dhtQueryTimeout = 1500 * time.Millisecond
)

// dhtListenPort TCP 监听端口（btengine 启动时置入，announce 用）
var dhtListenPort int

// btPeerAddr 一个候选 peer（wire 层的 host:port）
type btPeerAddr struct {
	Host string
	Port int
}

func (a btPeerAddr) String() string { return net.JoinHostPort(a.Host, strconv.Itoa(a.Port)) }

// dhtEntry 节点池条目：地址 + KID（idOK=false 表示还没学到对方 id）
type dhtEntry struct {
	id   [20]byte
	idOK bool
	addr btPeerAddr
}

// dhtNode 全局 DHT 实例
type dhtNode struct {
	mu     sync.Mutex
	conn   *net.UDPConn
	own    [20]byte // 自造 KID
	nodes  map[string]dhtEntry
	order  []string
	tokens map[string][]string // 节点 IP → 最近拿到的 announce token（最多 4 个）
	waits  map[string]chan map[string]any
	booted bool
}

var dhtG struct {
	once sync.Once
	n    *dhtNode
}

// dhtInit 启动全局 DHT（失败返回 nil：DHT 挂了只影响 peer 发现，主链路还有 tracker）
func dhtInit() *dhtNode {
	dhtG.once.Do(func() {
		n := &dhtNode{
			nodes:  map[string]dhtEntry{},
			tokens: map[string][]string{},
			waits:  map[string]chan map[string]any{},
		}
		rand.Read(n.own[:])
		conn, err := net.ListenUDP("udp", &net.UDPAddr{Port: 0})
		if err == nil {
			n.conn = conn
			go n.readLoop()
			n.bootstrap()
			dhtG.n = n
		}
	})
	return dhtG.n
}

// dhtN 取全局实例（可能 nil）
func dhtN() *dhtNode { return dhtG.n }

// dhtCloseForTest 测试封闭当前实例重建
func dhtCloseForTest() {
	n := dhtG.n
	if n != nil && n.conn != nil {
		n.conn.Close()
	}
	dhtG.n = nil
	dhtG.once = sync.Once{}
}

// dhtBootHosts 公共引导节点（多备几个：国内到 bittorrent.com 系 UDP 常丢包，单点全哑就废了）
var dhtBootHosts = []string{
	"router.bittorrent.com:6881",
	"router.utorrent.com:6881",
	"dht.transmissionbt.com:6881",
	"router.bitcomet.com:6881",
	"dht.libtorrent.org:25401",
	"dht.aelitis.com:6881",
}

// bootstrap 持续保活：每 60s 向全部 bootstrap 节点 ping 一轮（响应会 learnNode 入池）。
// 不做一次性 booted 置位——首启时若全部 ping 不通（或 DNS 抖动），一次性方案会让
// 节点池永远为空、findPeers 空转。每分钟 6 个 UDP 包的保活开销可忽略。
func (n *dhtNode) bootstrap() {
	n.mu.Lock()
	if n.booted {
		n.mu.Unlock()
		return
	}
	n.booted = true
	n.mu.Unlock()
	go func() {
		defer func() { recover() }()
		for {
			alive := n.poolSize()
			// 首启/池子小（<32）时每 30s 冲一轮；起来后退化为每 60s 保活
			interval := 60 * time.Second
			if alive < 32 {
				interval = 30 * time.Second
			}
			for _, host := range dhtBootHosts {
				if addr, err := net.ResolveUDPAddr("udp", host); err == nil {
					n.ping(addr)
				}
			}
			select {
			case <-time.After(interval):
			}
		}
	}()
}

// poolSize 当前节点池大小（诊断用）
func (n *dhtNode) poolSize() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.nodes)
}

func (n *dhtNode) readLoop() {
	if n.conn == nil {
		return
	}
	buf := make([]byte, 2048)
	for {
		c, raddr, err := n.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if c <= 0 {
			continue
		}
		pkt := make([]byte, c)
		copy(pkt, buf[:c])
		go func() {
			defer func() { recover() }()
			n.handle(pkt, raddr)
		}()
	}
}

// handle KRPC 包分派
func (n *dhtNode) handle(pkt []byte, raddr *net.UDPAddr) {
	v, _, err := btDec(pkt)
	if err != nil {
		return
	}
	m, isMap := v.(map[string]any)
	if !isMap {
		return
	}
	switch btStr(m, "y") {
	case "r", "e":
		n.onResp(m, raddr)
	case "q":
		n.onQuery(m, raddr)
	}
}

// onResp 响应：唤醒等待者 + 学节点（带 KID）+ 缓存 token
func (n *dhtNode) onResp(m map[string]any, raddr *net.UDPAddr) {
	respID := ""
	if r, ok := m["r"].(map[string]any); ok {
		if s, isStr := r["id"].(string); isStr {
			respID = s
		}
		if ns, ok := r["nodes"].(string); ok {
			for i := 0; i+26 <= len(ns); i += 26 {
				if p := decodeCompactNode(ns[i : i+26]); p != nil {
					n.learnNodeE(p, []byte(ns[i:i+20])) // 前 20B 是节点 KID
				}
			}
		}
		if tk := btStr(r, "token"); tk != "" && raddr != nil {
			key := raddr.IP.String()
			n.mu.Lock()
			n.tokens[key] = append(n.tokens[key], tk)
			if len(n.tokens[key]) > 4 {
				n.tokens[key] = n.tokens[key][1:]
			}
			n.mu.Unlock()
		}
	}
	n.learnNodeE(raddr, []byte(respID))
	if t := btStr(m, "t"); t != "" {
		n.mu.Lock()
		ch := n.waits[t]
		n.mu.Unlock()
		if ch != nil {
			select {
			case ch <- m:
			default:
			}
		}
	}
}

// onQuery 礼貌回复 ping / get_peers
func (n *dhtNode) onQuery(m map[string]any, raddr *net.UDPAddr) {
	qID := ""
	if a, ok := m["a"].(map[string]any); ok {
		if s, isStr := a["id"].(string); isStr {
			qID = s
		}
	}
	n.learnNodeE(raddr, []byte(qID))
	var r map[string]any
	switch btStr(m, "q") {
	case "get_peers":
		// 我们不提供 torrent 数据，回 nodes 空串 + 自造 token（	node 判定我们是终端）
		r = map[string]any{"id": n.own[:], "nodes": "", "token": n.tokenFor(raddr)}
	default:
		r = map[string]any{"id": n.own[:]}
	}
	t := btStr(m, "t")
	buf := btEnc(map[string]any{"t": t, "y": "r", "r": r})
	n.send(buf, raddr)
}

func (n *dhtNode) send(buf []byte, a *net.UDPAddr) {
	if n.conn == nil || a == nil {
		return
	}
	n.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	n.conn.WriteToUDP(buf, a)
	n.conn.SetWriteDeadline(time.Time{})
}

// tokenFor 生成给请求方的 announce token（滚动窗口，600s 换代）
func (n *dhtNode) tokenFor(a *net.UDPAddr) string {
	seed := time.Now().Unix() / 600
	return fmt.Sprintf("x%d%x", seed, a.IP[len(a.IP)-1])[:10]
}

// learnNodeE 收编节点并记录 KID（LRU 上限）；id 不足 20B 时仅学地址
func (n *dhtNode) learnNodeE(a *net.UDPAddr, id []byte) {
	if a == nil || a.Port <= 0 || a.Port > 65535 || a.IP == nil {
		return
	}
	ip := a.IP.String()
	if ip == "" || strings.HasPrefix(ip, "0.") {
		return
	}
	key := net.JoinHostPort(ip, strconv.Itoa(a.Port))
	n.mu.Lock()
	defer n.mu.Unlock()
	if e, ok := n.nodes[key]; !ok {
		e = dhtEntry{addr: btPeerAddr{Host: ip, Port: a.Port}}
		if len(id) == 20 {
			copy(e.id[:], id)
			e.idOK = true
		}
		n.nodes[key] = e
		n.order = append(n.order, key)
		if size := len(n.order); size > dhtMaxNodes {
			drop := n.order[0]
			n.order = n.order[1:]
			delete(n.nodes, drop)
		}
		return
	}
	// 已存在：补学 KID（先学到的节点后来才知道 id）→ 挪到队尾
	e := n.nodes[key]
	if !e.idOK && len(id) == 20 {
		copy(e.id[:], id)
		e.idOK = true
		n.nodes[key] = e
	}
	for i, k := range n.order {
		if k == key {
			copy(n.order[i:], n.order[i+1:])
			n.order[len(n.order)-1] = key
			break
		}
	}
}

// decodeCompactNode 26 字节 node 记录
func decodeCompactNode(b string) *net.UDPAddr {
	if len(b) != 26 {
		return nil
	}
	ip := net.IPv4(byte(b[20]), byte(b[21]), byte(b[22]), byte(b[23]))
	port := int(b[24])<<8 | int(b[25])
	return &net.UDPAddr{IP: ip, Port: port}
}

// txId 滚动事务号（3 字节 + 表绝不会撞）
var txMu sync.Mutex
var txSeq uint32

func txId() string {
	txMu.Lock()
	txSeq++
	v := txSeq ^ 0x5eed
	txMu.Unlock()
	b := make([]byte, 4)
	binary.BigEndian.PutUint32(b, v)
	return string(b[:3])
}

func (n *dhtNode) ping(a *net.UDPAddr) {
	n.queryMsg(a, "ping", map[string]any{"id": n.own[:]})
}

// getPeers 单次查询。注意：r["nodes"] 是 Kademlia 路由节点（不是该资源的 peer！），
// 节点只进池子供后续逼近查询，绝不喂给任务当 peer——v1.3.41 的「已发现 peer 25 但
// 全部抓取失败」根因就是把路由节点当 peer 拨号（它们大多 TCP 不开/不做种，注定失败），
// 而真 peer 在 values 字段里。节点本身由 onResp 捎带学习。
func (n *dhtNode) getPeers(a *net.UDPAddr, ih [20]byte) (peers []btPeerAddr, err error) {
	v, err := n.queryMsg(a, "get_peers", map[string]any{"id": n.own[:], "info_hash": ih[:]})
	if err != nil {
		return nil, err
	}
	r, isMap := v["r"].(map[string]any)
	if !isMap {
		return nil, fmt.Errorf("响应缺 r")
	}
	if vs, ok := r["values"].([]any); ok {
		for _, item := range vs {
			s, isStr := item.(string)
			if !isStr {
				continue
			}
			switch len(s) {
			case 6:
				peers = append(peers, decodeCompactPeer(s))
			case 18:
				for i := 0; i+18 <= len(s); i += 18 {
					peers = append(peers, decodeCompactPeerV6(s[i:i+18]))
				}
			}
		}
	}
	return peers, nil
}

// dhtXorLess 两个已知 KID 的节点到 target 的 XOR 距离比较（更小=更近=优先查）
func dhtXorLess(a, b dhtEntry, target [20]byte) bool {
	for i := 0; i < 20; i++ {
		xa, xb := a.id[i]^target[i], b.id[i]^target[i]
		if xa != xb {
			return xa < xb
		}
	}
	return false
}

func decodeCompactPeer(s string) btPeerAddr {
	ip := net.IPv4(byte(s[0]), byte(s[1]), byte(s[2]), byte(s[3]))
	port := int(s[4])<<8 | int(s[5])
	return btPeerAddr{Host: ip.String(), Port: port}
}

func decodeCompactPeerV6(s string) btPeerAddr {
	ip := net.IP(s[:16])
	port := int(s[16])<<8 | int(s[17])
	return btPeerAddr{Host: ip.String(), Port: port}
}

// announcePeer 用 get_peers 时攒下的 token 报到
func (n *dhtNode) announcePeer(a *net.UDPAddr, ih [20]byte, port int) {
	n.mu.Lock()
	tok := ""
	if a != nil && len(a.IP) > 0 {
		if l := n.tokens[a.IP.String()]; len(l) > 0 {
			tok = l[len(l)-1]
		}
	}
	n.mu.Unlock()
	if tok == "" {
		return
	}
	n.queryMsg(a, "announce_peer", map[string]any{
		"id": n.own[:], "info_hash": ih[:], "port": port, "token": tok,
	})
}

// queryMsg 发查询并等响应，双轨等 + 顶层 recover 防炸弹
func (n *dhtNode) queryMsg(a *net.UDPAddr, q string, args map[string]any) (map[string]any, error) {
	if n.conn == nil || a == nil {
		return nil, fmt.Errorf("DHT 未就绪")
	}
	tid := txId()
	ch := make(chan map[string]any, 2)
	n.mu.Lock()
	n.waits[tid] = ch
	n.mu.Unlock()
	defer func() {
		n.mu.Lock()
		delete(n.waits, tid)
		n.mu.Unlock()
	}()
	buf := btEnc(map[string]any{"t": tid, "y": "q", "q": q, "a": args})
	for attempt := 0; attempt < 2; attempt++ {
		n.send(buf, a)
		select {
		case m := <-ch:
			if btStr(m, "y") == "e" {
				return nil, fmt.Errorf("DHT 拒绝")
			}
			return m, nil
		case <-time.After(dhtQueryTimeout):
			// 第二次尝试（UAT 掉包补救）
		}
	}
	return nil, fmt.Errorf("DHT 响应超时")
}

// findPeers 任务级 Kademlia 逼近查询：每轮从池子里挑「离目标 infohash 最近」的
// 未试节点批量 get_peers；响应里学到的新节点（更靠近目标）下一轮自然优先。
// 全池试完还没新节点就歇 3s 重置再扫（顺带探活）。直到 ctx 结束。
func (n *dhtNode) findPeers(taskCtx context.Context, ih [20]byte, out func(btPeerAddr)) {
	defer func() { recover() }()
	tried := map[string]bool{}
	const dhtWalkBatch = 16
	for {
		// 候选：池内全部可查节点，按 XOR 距离排序（无 KID 的排最后，稳定序）
		n.mu.Lock()
		cand := make([]dhtEntry, 0, len(n.nodes))
		for _, e := range n.nodes {
			if e.addr.Port > 0 && !tried[e.addr.String()] {
				cand = append(cand, e)
			}
		}
		n.mu.Unlock()
		sort.SliceStable(cand, func(i, j int) bool {
			ci, cj := cand[i], cand[j]
			switch {
			case ci.idOK && cj.idOK:
				return dhtXorLess(ci, cj, ih)
			case ci.idOK:
				return true
			case cj.idOK:
				return false
			}
			return false
		})
		if len(cand) > dhtWalkBatch {
			cand = cand[:dhtWalkBatch]
		}
		for _, e := range cand {
			if taskCtx.Err() != nil {
				break
			}
			tried[e.addr.String()] = true
			up, err := net.ResolveUDPAddr("udp", e.addr.String())
			if err != nil {
				continue
			}
			peers, err := n.getPeers(up, ih)
			if err != nil {
				continue
			}
			for _, p := range peers {
				if p.Port > 0 {
					out(p)
				}
			}
		}
		if taskCtx.Err() != nil {
			// 结束时做种回报（好公民）
			go n.announceAll(ih)
			return
		}
		if len(cand) == 0 {
			select {
			case <-taskCtx.Done():
				go n.announceAll(ih)
				return
			case <-time.After(3 * time.Second):
				tried = map[string]bool{} // 全试过：歇片刻重置再扫
			}
		} else {
			select {
			case <-taskCtx.Done():
				go n.announceAll(ih)
				return
			case <-time.After(time.Second):
			}
		}
	}
}

// announceAll 完成回报（尽力而为，走热点节点）
func (n *dhtNode) announceAll(ih [20]byte) {
	defer func() { recover() }()
	n.mu.Lock()
	hot := make([]btPeerAddr, 0, 8)
	for i := len(n.order) - 1; i >= 0 && len(hot) < 8; i-- {
		if e, ok := n.nodes[n.order[i]]; ok && e.addr.Port > 0 {
			hot = append(hot, e.addr)
		}
	}
	n.mu.Unlock()
	for _, a := range hot {
		up, err := net.ResolveUDPAddr("udp", a.String())
		if err != nil {
			continue
		}
		n.announcePeer(up, ih, dhtListenPort)
	}
}
