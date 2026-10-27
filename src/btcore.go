package main

// 内置 BT 引擎 · 基础层：bencode 编解码、磁力链接解析、metainfo（种子元数据）。
//
// 依赖约束：项目零第三方库，全部用标准库手写（BitTorrent BEP3/9/10/23 的必要子集）。
// 层次：
//   btcore.go  —— bencode / magnet / info dict（本文件）
//   btdht.go   —— DHT（KRPC UDP）peer 发现
//   btwire.go  —— peer wire TCP（handshake / 扩展握手 / ut_metadata / piece 传输）
//   btengine.go—— 任务状态机 / 文件落盘 / 进度速度 / 对外 API 视图
//
// 安全基线（与下载模块红线一致）：
//   - 所有解析有长度上限（防畸形数据打爆内存）；
//   - 目录校验走 safeDownloadDir；文件名一律 btCleanName 清洗后落盘；
//   - piece 校验失败绝不写正盘文件（先分块缓冲，验完才 WriteAt）。

import (
	"crypto/sha1"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ---------- bencode ----------

const btMaxDecode = 8 << 20 // 单次 bencode 解析上限 8MB（ut_metadata 逐段拼接后限时一次性解析，16MB 级 metainfo 也够用再大不合常理）

// btDec 反 bencode：返回值 + 消耗字节数。
// 整数 → int64；字符串 → string；列表 → []any；字典 → map[string]any。
func btDec(b []byte) (any, int, error) {
	if len(b) > btMaxDecode {
		return nil, 0, fmt.Errorf("数据超限")
	}
	v, n, err := btDecVal(b, 0)
	return v, n, err
}

func btDecVal(b []byte, i int) (any, int, error) {
	if i >= len(b) {
		return nil, 0, fmt.Errorf("bencode 截断")
	}
	switch c := b[i]; {
	case c >= '0' && c <= '9':
		// 字符串：<len>:<bytes>
		cs := i
		for i < len(b) && b[i] != ':' {
			i++
			if i-cs > 12 {
				return nil, 0, fmt.Errorf("字符串长度头过长")
			}
		}
		if i >= len(b) {
			return nil, 0, fmt.Errorf("bencode 截断")
		}
		ln, err := strconv.Atoi(string(b[cs:i]))
		if err != nil || ln < 0 {
			return nil, 0, fmt.Errorf("字符串长度非法")
		}
		i++ // 跳过 ':'
		if i+ln > len(b) {
			return nil, 0, fmt.Errorf("字符串截断")
		}
		return string(b[i : i+ln]), i + ln, nil
	case c == 'i':
		// 整数：i<digits>e（BT 规范允许带负号）
		i++
		c0 := i
		for i < len(b) && b[i] != 'e' {
			i++
			if i-c0 > 21 {
				return nil, 0, fmt.Errorf("整数过长")
			}
		}
		if i >= len(b) {
			return nil, 0, fmt.Errorf("bencode 截断")
		}
		v, err := strconv.ParseInt(string(b[c0:i]), 10, 64)
		if err != nil {
			return nil, 0, fmt.Errorf("整数非法")
		}
		return v, i + 1, nil
	case c == 'l':
		// 列表
		i++
		out := []any{}
		for i < len(b) && b[i] != 'e' {
			v, n, err := btDecVal(b, i)
			if err != nil {
				return nil, 0, err
			}
			out = append(out, v)
			i = n
		}
		if i >= len(b) {
			return nil, 0, fmt.Errorf("bencode 截断")
		}
		return out, i + 1, nil
	case c == 'd':
		// 字典（key 必为字符串；重复 key 后者胜，非致命）
		i++
		out := map[string]any{}
		for i < len(b) && b[i] != 'e' {
			if b[i] < '0' || b[i] > '9' {
				return nil, 0, fmt.Errorf("字典 key 非字符串")
			}
			kv, n, err := btDecVal(b, i)
			if err != nil {
				return nil, 0, err
			}
			i = n
			ks, isStr := kv.(string)
			if !isStr {
				return nil, 0, fmt.Errorf("字典 key 非字符串")
			}
			v, n, err := btDecVal(b, i)
			if err != nil {
				return nil, 0, err
			}
			out[ks] = v
			i = n
		}
		if i >= len(b) {
			return nil, 0, fmt.Errorf("bencode 截断")
		}
		return out, i + 1, nil
	default:
		return nil, 0, fmt.Errorf("bencode 非法字符 %q", c)
	}
}

// btEnc 编码 any（int64/int → i；string → s；[]any → l；map[string]any → d）
// 字典 key 按 bencode 规范（字典序）输出——注意！infohash 必须对「原始字节」重新编码，
// 这里只对从原始数据里抽取的 info dict 重编码：字符串值原样进 dict，等价于原 dict。
func btEnc(v any) []byte {
	buf := make([]byte, 0, 512)
	return btEncVal(buf, v)
}

func btEncVal(buf []byte, v any) []byte {
	switch x := v.(type) {
	case int:
		return btEncVal(buf, int64(x))
	case int64:
		buf = append(buf, 'i')
		buf = append(buf, strconv.FormatInt(x, 10)...)
		return append(buf, 'e')
	case string:
		buf = append(buf, strconv.Itoa(len(x))...)
		buf = append(buf, ':')
		return append(buf, x...)
	case []any:
		buf = append(buf, 'l')
		for _, e := range x {
			buf = btEncVal(buf, e)
		}
		return append(buf, 'e')
	case map[string]any:
		buf = append(buf, 'd')
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			buf = btEncVal(buf, k)
			buf = btEncVal(buf, x[k])
		}
		return append(buf, 'e')
	default:
		return buf
	}
}

// btInt dict 取整数（缺省 0）
func btInt(m map[string]any, key string) int64 {
	if v, ok := m[key]; ok {
		switch x := v.(type) {
		case int64:
			return x
		case int:
			return int64(x)
		}
	}
	return 0
}

// btStr dict 取字符串
func btStr(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// ---------- 磁力链接 ----------

// reBtihHex 40 位 hex infohash；reBtihB32 32 位 base32
var (
	reBtihHex = regexp.MustCompile(`xt=urn:btih:([a-fA-F0-9]{40})`)
	reBtihB32 = regexp.MustCompile(`xt=urn:btih:([A-Za-z2-7]{32})(&|$)`)
)

// btMagnet 磁力解析：infohash（统一 hex 大写）、显示名 dn、tracker 列表 tr
func btMagnet(magnet string) (hash [20]byte, dn string, tr []string, err error) {
	m := reBtihHex.FindStringSubmatch(magnet)
	if m != nil {
		raw, _ := hex.DecodeString(strings.ToLower(m[1]))
		copy(hash[:], raw)
	} else if m = reBtihB32.FindStringSubmatch(magnet); m != nil {
		raw, _ := base32.StdEncoding.DecodeString(strings.ToUpper(m[1]))
		copy(hash[:], raw)
	} else {
		return hash, "", nil, fmt.Errorf("磁力链接缺少有效 infohash")
	}
	u, perr := url.Parse(magnet)
	if perr == nil {
		if s := strings.TrimSpace(u.Query().Get("dn")); s != "" {
			dn = s
		}
		for _, t := range u.Query()["tr"] {
			if t = strings.TrimSpace(t); t != "" {
				tr = append(tr, t)
			}
		}
	} else {
		// 极端情况下 url.Parse 失败，dn 退化手抓（吸血磁力也可能带 & 分号）
		if q := strings.SplitN(magnet, "dn=", 2); len(q) == 2 {
			dn = strings.SplitN(q[1], "&", 2)[0]
		}
	}
	return hash, dn, tr, nil
}

// btHashHex infohash → hex 字符串（小写，用于去重/id）
func btHashHex(h [20]byte) string {
	return hex.EncodeToString(h[:])
}

// ---------- metainfo（info dict） ----------

// btFile 种子内一个文件
type btFile struct {
	Path string `json:"path"` // 相对种子名的各层目录已拼好（含文件名）
	Size int64  `json:"size"`
}

// btMeta 种子元信息
type btMeta struct {
	Name     string   `json:"name"`
	PieceLen int64    `json:"pieceLen"`
	Pieces   []byte   `json:"-"` // 每片 20 字节 SHA1，拼在一起
	Len      int64    `json:"len"` // 单文件种子总长（多文件为 0）
	Files    []btFile `json:"files,omitempty"`
	raw      []byte   `json:"-"` // info dict 的 bencode 原文（元数据缓存落盘用，SHA1==infohash）
}

// btParseInfo 解析 info dict；返回元信息与原始编码字节（供重算校验 infohash）
func btParseInfo(info any) (*btMeta, []byte, error) {
	d, isMap := info.(map[string]any)
	if !isMap {
		return nil, nil, fmt.Errorf("info 非字典")
	}
	raw := btEnc(d)
	if len(d["pieces"].(string))%20 != 0 && d["pieces"] == nil {
		return nil, nil, fmt.Errorf("info 缺 pieces")
	}
	m := &btMeta{
		Name:     btCleanName(btStr(d, "name")),
		PieceLen: btInt(d, "piece length"),
	}
	if m.Name == "" {
		return nil, nil, fmt.Errorf("info 缺 name")
	}
	if m.PieceLen <= 0 || m.PieceLen > (64<<20) {
		return nil, nil, fmt.Errorf("piece length 异常")
	}
	m.Pieces = []byte(btStr(d, "pieces"))
	if len(m.Pieces) == 0 || len(m.Pieces)%20 != 0 {
		return nil, nil, fmt.Errorf("pieces 数据异常")
	}
	if fl, ok := d["files"].([]any); ok {
		// 多文件种子
		for _, f := range fl {
			fd, isMap := f.(map[string]any)
			if !isMap {
				return nil, nil, fmt.Errorf("files 条目非字典")
			}
			parts, isList := fd["path"].([]any)
			if !isList || len(parts) == 0 {
				return nil, nil, fmt.Errorf("files 条目缺 path")
			}
			var segs []string
			for _, p := range parts {
				segs = append(segs, btCleanName(ofAnyStr(p)))
			}
			m.Files = append(m.Files, btFile{
				Path: strings.Join(segs, "/"),
				Size: btInt(fd, "length"),
			})
		}
		if len(m.Files) == 0 {
			return nil, nil, fmt.Errorf("files 为空")
		}
	} else {
		m.Len = btInt(d, "length")
		if m.Len < 0 {
			return nil, nil, fmt.Errorf("length 异常")
		}
	}
	m.raw = raw // 供元数据缓存落盘（SHA1(raw)==infohash，已由调用方核对）
	return m, raw, nil
}

// ofAnyStr []any 里的字符串元素
func ofAnyStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// btNumPieces 总片数
func (m *btMeta) btNumPieces() int {
	return len(m.Pieces) / 20
}

// btPieceSha1 第 i 片的校验值
func (m *btMeta) btPieceSha1(i int) [20]byte {
	var h [20]byte
	copy(h[:], m.Pieces[i*20:i*20+20])
	return h
}

// btTotalSize 总字节数（兼容单/多文件）
func (m *btMeta) btTotalSize() int64 {
	if len(m.Files) > 0 {
		var n int64
		for _, f := range m.Files {
			n += f.Size
		}
		return n
	}
	return m.Len
}

// btHashInfo 重算 info dict 的 infohash（身份校验：ut_metadata 拼出的 info 必须 SHA1 相等）
func btHashInfo(raw []byte) [20]byte {
	return sha1.Sum(raw)
}

var reBtBadName = regexp.MustCompile(`[\\/:*?"<>|\x00-\x1f]|\.\.+|^\.|^/|/$`)

// btCleanName 种子内文件路径净化：保留中英数与常见符号，限制层级逃逸
func btCleanName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "..", "_")
	s = reBtBadName.ReplaceAllString(s, " ")
	out := strings.TrimSpace(s)
	out = strings.Join(strings.Fields(out), " ")
	r := []rune(out)
	if len(r) > 120 {
		out = string(r[:120])
	}
	return out
}
