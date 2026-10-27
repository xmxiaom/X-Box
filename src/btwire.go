package main

// 内置 BT 引擎 · peer wire TCP 层（BEP3 + BEP10 扩展握手 + BEP9 ut_metadata）。
//
// 本层职责（任务状态机在 btengine.go）：
//   - btHandshake：出站握手（以出站为主，NAS 在 NAT 后纯出站也能下载）
//   - btFetchMetadata：向一个 peer 取元数据（BEP9），拼回 info dict 并 SHA1 与磁力核对
//   - 帧读写原语 btReadMsg/btWriteMsg（piece 下载 worker 也复用）
//
// 协议备忘（调试对照）：
//   handshake: 19 + "BitTorrent protocol" + reserved[8] + infohash[20] + peerid[20]
//   消息帧   : 4B 大端长度 + type(1B) + payload；type: 0 choke 1 unchoke 2 interested
//              3 not interested 4 have 5 bitfield 6 request 7 piece 8 cancel
//   extended : type=20，payload 首字节=扩展 id（0=握手），后接 bencode 字典 + 可选尾巴
//   ut_metadata: {msg_type:0请求/1数据/2拒, piece, [total_size]} + 数据尾巴

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strconv"
	"time"
)

const (
	btProtocol     = "BitTorrent protocol"
	btReserved     = "\x00\x00\x00\x00\x00\x10\x00\x03" // DHT(0x01) + extended(0x10)
	btMaxMsg       = 1 << 20                            // 单帧上限 1MB（piece 块 ≤ 64KB，元数据片 16KB）
	btMetaPieceLen = 16 << 10                           // 元数据分片 16KB
	btMaxMetaSize  = 16 << 20                           // 元数据上限 16MB
	btIOTimeout    = 45 * time.Second
	btMetaPeerCap  = 30 * time.Second // 单个 peer 抓元数据全程上限（拨+握+取）；压制坏 peer 占坑
)

// btSelfID 进程级 peer id（-XB 前缀 + 随机 16 字节）
var btSelfID = func() [20]byte {
	var b [20]byte
	copy(b[:], "-XB-")
	rand.Read(b[4:])
	return b
}()

// btHandshake 出站握手 → 校验为 BT 协议后返回可用连接
func btHandshake(conn net.Conn, ih [20]byte) (net.Conn, error) {
	conn.SetDeadline(time.Now().Add(8 * time.Second))
	hs := make([]byte, 0, 68)
	hs = append(hs, 19)
	hs = append(hs, btProtocol...)
	hs = append(hs, btReserved...)
	hs = append(hs, ih[:]...)
	hs = append(hs, btSelfID[:]...)
	if _, err := conn.Write(hs); err != nil {
		return nil, err
	}
	resp := make([]byte, 68)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return nil, fmt.Errorf("握手响应不完整")
	}
	if resp[0] != 19 || string(resp[1:20]) != btProtocol {
		return nil, fmt.Errorf("不是 BitTorrent 协议")
	}
	conn.SetDeadline(time.Time{})
	return conn, nil
}

// ---------- 消息帧 ----------

// btReadMsg 读一帧。keep-alive（长度 0）返回 typ=0xFF。
func btReadMsg(conn net.Conn) (typ byte, payload []byte, err error) {
	return btReadMsgUntil(conn, time.Time{})
}

// btReadMsgUntil 读一帧，帧内每次 IO 的 deadline = min(45s, until)。
// until 为零表示不限（用默认 45s）。
func btReadMsgUntil(conn net.Conn, until time.Time) (typ byte, payload []byte, err error) {
	d := time.Now().Add(btIOTimeout)
	if !until.IsZero() && until.Before(d) {
		d = until
	}
	if err := conn.SetReadDeadline(d); err != nil {
		return 0, nil, err
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(conn, head); err != nil {
		return 0, nil, err
	}
	ln := int(head[0])<<24 | int(head[1])<<16 | int(head[2])<<8 | int(head[3])
	if ln == 0 {
		return 0xFF, nil, nil
	}
	if ln < 0 || ln > btMaxMsg {
		return 0, nil, fmt.Errorf("帧长异常 %d", ln)
	}
	pl := make([]byte, ln)
	if _, err := io.ReadFull(conn, pl); err != nil {
		return 0, nil, err
	}
	conn.SetReadDeadline(time.Time{})
	return pl[0], pl[1:], nil
}

// btWriteMsg 写一帧
func btWriteMsg(conn net.Conn, typ byte, payload []byte) error {
	if err := conn.SetWriteDeadline(time.Now().Add(btIOTimeout)); err != nil {
		return err
	}
	n := len(payload) + 1
	buf := make([]byte, 4, 4+n)
	buf[0] = byte(n >> 24)
	buf[1] = byte(n >> 16)
	buf[2] = byte(n >> 8)
	buf[3] = byte(n)
	buf = append(buf, typ)
	buf = append(buf, payload...)
	_, err := conn.Write(buf)
	return err
}

// btJoinExt 扩展帧 payload：id 头 + bencode 字典 (+ 尾巴)
func btJoinExt(id byte, dict map[string]any, tail []byte) []byte {
	payload := append([]byte{id}, btEnc(dict)...)
	return append(payload, tail...)
}

// btSplitExt 扩展帧 payload → 字典 + 二进制尾巴
func btSplitExt(payload []byte) (dict map[string]any, tail []byte, err error) {
	if len(payload) < 1 {
		return nil, nil, fmt.Errorf("扩展帧过短")
	}
	v, n, err := btDec(payload[1:])
	if err != nil {
		return nil, nil, err
	}
	d, isMap := v.(map[string]any)
	if !isMap {
		return nil, nil, fmt.Errorf("扩展帧非字典")
	}
	return d, payload[1+n:], nil
}

// ---------- BEP10 扩展握手 ----------

// btExtInfo 对方扩展握手摘要
type btExtInfo struct {
	MetadataID   byte // 对方 ut_metadata 扩展 id（0=不支持）
	MetadataSize int
}

// btExtParse 解析 id=0 的扩展握手
func btExtParse(dict map[string]any) btExtInfo {
	out := btExtInfo{}
	if mm, ok := dict["m"].(map[string]any); ok {
		if id, ok := mm["ut_metadata"].(int64); ok && id > 0 && id < 255 {
			out.MetadataID = byte(id)
		}
	}
	out.MetadataSize = int(btInt(dict, "metadata_size"))
	return out
}

// ---------- BEP9 ut_metadata 元数据抓取 ----------

// btMetaState BEP9 元数据抓取桩
type btMetaState struct {
	peerID  byte
	size    int
	data    []byte
	have    []bool
	pending int
}

// btFetchMetaFromPeer 向一个 peer 抓元数据并校验（全自动：握手 → 扩展握手 →
// 请求缺片 → 拼齐 → SHA1 比对 → 解析）。单 peer 全程 ≤ btMetaPeerCap，
// 失败原因分类计数由引擎聚合进任务错误消息。
func btFetchMetaFromPeer(ctx context.Context, addr string, ih [20]byte) (*btMeta, error) {
	cctx, cancel := context.WithTimeout(ctx, btMetaPeerCap)
	defer cancel()
	m, err := btFetchMetaFromPeerRaw(cctx, addr, ih)
	if os.Getenv("XBOX_DEV") == "1" {
		if err != nil {
			fmt.Fprintln(os.Stderr, "[btmeta] FAIL", addr, err.Error())
		} else if m != nil {
			fmt.Fprintf(os.Stderr, "[btmeta] OK  %s size=%d files=%d\n", addr, m.Len, len(m.Files))
		}
	}
	return m, err
}

func btFetchMetaFromPeerRaw(ctx context.Context, addr string, ih [20]byte) (*btMeta, error) {
	mode := 0
	if v, aerr := strconv.Atoi(os.Getenv("XBOX_META_DIAG_MODE")); aerr == nil {
		mode = v
	}
	verbose := false
	if vp := os.Getenv("XBOX_META_DIAG_VERBOSE"); vp != "" && os.Getenv("XBOX_DEV") == "1" {
		verbose = regexp.MustCompile(vp).MatchString(addr)
	}
	dialer := &net.Dialer{Timeout: 6 * time.Second}
	dialOK := time.Now()
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if verbose {
		fmt.Fprintf(os.Stderr, "[stage] %s dial %.1fs\n", addr, time.Since(dialOK).Seconds())
	}
	if _, err := btHandshake(conn, ih); err != nil {
		return nil, err
	}
	if verbose {
		fmt.Fprintf(os.Stderr, "[stage] %s handshake ok\n", addr)
	}
	// 我们声明支持 ut_metadata（对方未必用得上，但规范上先广播）
	switch {
	case mode == 1: // 不发扩展握手，只读帧
	case mode == 2: // 字面 bencode 扩展握手（与 diag2 完全一致）
		if err := btWriteMsg(conn, 20, append([]byte{0},
			[]byte("d1:md12:ut_metadatai1eee1:p68811:v6:diagt1e")...)); err != nil {
			return nil, err
		}
	default: // 引擎标准路径
		if err := btWriteMsg(conn, 20, btJoinExt(0, map[string]any{
			"m":             map[string]any{"ut_metadata": 1},
			"p":             dhtListenPort,
			"v":             "X-Box " + version,
			"metadata_size": 0,
		}, nil)); err != nil {
			return nil, err
		}
	}
	if verbose && mode != 1 {
		fmt.Fprintf(os.Stderr, "[stage] %s ext-hs sent\n", addr)
	}
	var ms btMetaState
	until, _ := ctx.Deadline() // 单 peer 上限（外层 WithTimeout 保证非零）
	if until.IsZero() {
		until = time.Now().Add(60 * time.Second)
	}
	// 逐帧循环直到拼齐或败
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if time.Now().After(until) {
			return nil, fmt.Errorf("元数据获取超时")
		}
		typ, pl, err := btReadMsgUntil(conn, until)
		if verbose {
			fmt.Fprintf(os.Stderr, "[stage] %s frame typ=%d len=%d err=%v\n", addr, typ, len(pl), err)
		}
		if err != nil {
			return nil, err
		}
		if typ == 0xFF { // keep-alive，继续等
			continue
		}
		switch typ {
		case 20: // 扩展
			if verbose {
				hl := len(pl)
				if hl > 120 {
					hl = 120
				}
				fmt.Fprintf(os.Stderr, "[stage] %s extframe id=%d head=%q\n", addr, pl[0], string(pl[1:hl]))
			}
			if len(pl) == 0 {
				continue
			}
			if pl[0] == 0 {
				ext := btExtParse(mchieveDict(pl[1:]))
				if ext.MetadataID == 0 {
					return nil, fmt.Errorf("对方不支持 ut_metadata")
				}
				if ext.MetadataSize <= 0 || ext.MetadataSize > btMaxMetaSize {
					return nil, fmt.Errorf("元数据大小异常 %d", ext.MetadataSize)
				}
				ms.peerID = ext.MetadataID
				ms.size = ext.MetadataSize
				ms.data = make([]byte, ms.size)
				ms.have = make([]bool, (ms.size+btMetaPieceLen-1)/btMetaPieceLen)
				// 保底请出替换（后续收到一片再补发下一批）
				if err := reqMissingMeta(conn, &ms); err != nil {
					return nil, err
				}
			} else {
				// 部分客户端（libtorrent 系）回数据帧标的是我方广告的扩展 id 而非
				// 其自报 id，故按帧内容识别 ut_metadata（msg_type 1/2 + piece），
				// 不做 pl[0]==ms.peerID 严格断定；其它扩展（如 ut_pex 无 msg_type）自然放过。
				d, tail, err := btSplitExt(pl)
				if err != nil {
					return nil, err
				}
				if verbose {
					fmt.Fprintf(os.Stderr, "[stage] %s meta-dict msg_type=%d piece=%d total_size=%d taillen=%d have=%v pend=%d\n",
						addr, btInt(d, "msg_type"), btInt(d, "piece"), btInt(d, "total_size"), len(tail), ms.have, ms.pending)
				}
				if int(btInt(d, "msg_type")) == 2 {
					return nil, fmt.Errorf("对方拒绝元数据请求")
				}
				if int(btInt(d, "msg_type")) != 1 {
					continue
				}
				pi := int(btInt(d, "piece"))
				if pi < 0 || pi >= len(ms.have) || ms.have[pi] {
					ms.pending--
					continue
				}
				off := pi * btMetaPieceLen
				if off+len(tail) > ms.size {
					continue
				}
				copy(ms.data[off:off+len(tail)], tail)
				ms.have[pi] = true
				ms.pending--
				reqMissingMeta(conn, &ms)
				if allHave(ms.have) {
					return btParseMetaFromRaw(ms.data, ih)
				}
			}
		}
	}
}

// mchieveDict 拆扩展握手（容错：格式不对返回空 dict）
func mchieveDict(pl []byte) map[string]any {
	d, _, err := btSplitExt(append([]byte{0}, pl...))
	if err != nil || d == nil {
		return map[string]any{}
	}
	return d
}

// allHave 全收齐？
func allHave(h []bool) bool {
	for _, b := range h {
		if !b {
			return false
		}
	}
	return true
}

// reqMissingMeta 补发缺失片（一次填满 8 个挂起请求；v1.3.41 及之前每轮只发 1 个，
// 管线深度 1，每个元数据片都要等一个 RTT，慢元数据容易撞 60s 窗口）
func reqMissingMeta(conn net.Conn, ms *btMetaState) error {
	flight := ms.pending
	for i := 0; i < len(ms.have) && flight < 8; i++ {
		if ms.have[i] {
			continue
		}
		req := btEnc(map[string]any{"msg_type": 0, "piece": i})
		if err := btWriteMsg(conn, 20, append([]byte{ms.peerID}, req...)); err != nil {
			return err
		}
		flight++
		ms.pending = flight
	}
	return nil
}

// btParseMetaFromRaw 元数据原文校验 + 解析（info 必须是 bencode dict 原文）
func btParseMetaFromRaw(raw []byte, ih [20]byte) (*btMeta, error) {
	if btHashInfo(raw) != ih {
		return nil, fmt.Errorf("infohash 不匹配（元数据损坏或异种）")
	}
	info, _, err := btDec(raw)
	if err != nil {
		return nil, err
	}
	meta, _, err := btParseInfo(info)
	return meta, err
}
