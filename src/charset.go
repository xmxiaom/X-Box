package main

// 乱码修复：处理「GBK 字节被当成单字节西文解码」的文本。
//
// 中文音乐库里很常见：早年工具把 GBK 的标签/文件名按 ISO-8859-1 或 CP1252
// 逐字节转成字符，于是「阿悄」变成「°¢ÇÄ」。这类字符串本身是合法 UTF-8，
// 只是内容错了，靠格式嗅探发现不了，只能按「高位字符比例 + 还原后是否成中文」
// 来判定并回退。
//
// 纯标准库实现：GBK 映射表在 charset_gbk.go（生成文件），不引第三方包。

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// gbkDecode 严格解码 GBK 字节；遇到未映射字节即失败（宁可不解也不乱解）
func gbkDecode(b []byte) (string, bool) {
	var sb strings.Builder
	for i := 0; i < len(b); {
		c := b[i]
		if c < 0x80 {
			sb.WriteByte(c)
			i++
			continue
		}
		if c < gbkLead0 || i+1 >= len(b) {
			return "", false
		}
		t := b[i+1]
		if t < gbkTrail0 || t > 0xFE {
			return "", false
		}
		idx := int(c-gbkLead0)*gbkTrailWidth + int(t-gbkTrail0)
		if idx < 0 || idx >= len(gbkTable) {
			return "", false
		}
		r := gbkTable[idx]
		if r == 0 {
			return "", false
		}
		sb.WriteRune(rune(r))
		i += 2
	}
	return sb.String(), true
}

// singleByteRune 单字节西文编码的反查表：Unicode → 原始字节。
// 同时收 CP1252（Windows 西欧）与 CP1250（中欧，0xD0 是 Đ）、
// 以及 Latin-2 特有字母，覆盖常见的「按哪种单字节页解码」情形。
var singleByteRune = func() map[rune]byte {
	m := map[rune]byte{}
	for i := 0; i < 0x80; i++ {
		m[rune(i)] = byte(i)
	}
	for i := 0xA0; i <= 0xFF; i++ {
		m[rune(i)] = byte(i)
	}
	cp1252 := [32]rune{
		0x20AC, 0x0081, 0x201A, 0x0192, 0x201E, 0x2026, 0x2020, 0x2021,
		0x02C6, 0x2030, 0x0160, 0x2039, 0x0152, 0x008D, 0x017D, 0x008F,
		0x0090, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014,
		0x02DC, 0x2122, 0x0161, 0x203A, 0x0153, 0x009D, 0x017E, 0x0178,
	}
	cp1250 := [32]rune{
		0x20AC, 0x0081, 0x201A, 0x0083, 0x201E, 0x2026, 0x2020, 0x2021,
		0x0088, 0x2030, 0x0160, 0x2039, 0x015A, 0x0164, 0x017D, 0x0179,
		0x0090, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014,
		0x0098, 0x2122, 0x0161, 0x203A, 0x015B, 0x0165, 0x017E, 0x017A,
	}
	for i, r := range cp1252 {
		if r >= 0x80 {
			m[r] = byte(0x80 + i)
		}
	}
	for i, r := range cp1250 {
		if r >= 0x80 {
			m[r] = byte(0x80 + i)
		}
	}
	// Latin-2 / 中欧字母（CP1250 高位区），0x0110=Đ 这类不在一字节页里的字母
	latin2 := map[rune]byte{
		0x0104: 0xA1, 0x0105: 0xB1, 0x010C: 0xC8, 0x010D: 0xE8, 0x010E: 0xCF, 0x010F: 0xEF,
		0x0110: 0xD0, 0x0111: 0xF0, 0x0118: 0xCA, 0x0119: 0xEA, 0x011A: 0xCC, 0x011B: 0xEC,
		0x0139: 0xC5, 0x013A: 0xE5, 0x013D: 0xBC, 0x013E: 0xBE, 0x0141: 0xA3, 0x0142: 0xB3,
		0x0143: 0xD1, 0x0144: 0xF1, 0x0147: 0xD2, 0x0148: 0xF2, 0x0150: 0xD5, 0x0151: 0xF5,
		0x0154: 0xC0, 0x0155: 0xE0, 0x0158: 0xD8, 0x0159: 0xF8, 0x015E: 0xAA, 0x015F: 0xBA,
		0x0162: 0xDE, 0x0163: 0xFE, 0x016E: 0xD9, 0x016F: 0xF9, 0x0170: 0xDB, 0x0171: 0xFB,
		0x017B: 0xAF, 0x017C: 0xBF,
	}
	for r, b := range latin2 {
		m[r] = b
	}
	return m
}()

// mojibakeToBytes 把「按单字节西文解码后的字符串」还原成原始字节
func mojibakeToBytes(s string) ([]byte, bool) {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		b, ok := singleByteRune[r]
		if !ok {
			return nil, false
		}
		out = append(out, b)
	}
	return out, true
}

// hanCount 统计汉字个数
func hanCount(s string) int {
	n := 0
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			n++
		}
	}
	return n
}

// looksMojibake 粗判是否像「单字节解码造成的乱码」：
// 没有任何汉字、却有多个高位（>=0x80）字符
func looksMojibake(s string) bool {
	if s == "" || hanCount(s) > 0 {
		return false
	}
	high := 0
	for _, r := range s {
		if r >= 0x80 {
			high++
		}
	}
	return high >= 2
}

// fixMojibake 尝试把乱码文本还原成中文；判定不了就原样返回。
// 只在「原串无汉字」且「还原后汉字占多数」时才替换，避免误伤正常外文。
func fixMojibake(s string) string {
	if s == "" {
		return s
	}
	// 情况 A：字符串里直接是非法 UTF-8 的 GBK 原始字节
	if !utf8.ValidString(s) {
		if dec, ok := gbkDecode([]byte(s)); ok && hanCount(dec) > 0 {
			return dec
		}
		return s
	}
	if !looksMojibake(s) {
		return s
	}
	b, ok := mojibakeToBytes(s)
	if !ok {
		return s
	}
	best, bestHan := s, hanCount(s)
	// 候选 1：原始字节是 GBK（最常见：中文工具写的标签被按西文读）
	if dec, ok := gbkDecode(b); ok && hanCount(dec) > bestHan {
		best, bestHan = dec, hanCount(dec)
	}
	// 候选 2：原始字节其实是 UTF-8（被按西文页解了两遍）
	if utf8.Valid(b) {
		if dec := string(b); hanCount(dec) > bestHan {
			best, bestHan = dec, hanCount(dec)
		}
	}
	if best == s || bestHan < 2 {
		return s
	}
	// 汉字要占多数，否则可能只是含少量中文的外文名，保持原样
	if bestHan*2 < utf8.RuneCountInString(best) {
		return s
	}
	return best
}
