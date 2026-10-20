// MP4 / MOV / M4A 容器解析与标签写入
//
// 盒结构：ftyp → moov → (mvhd, trak→tkhd, udta→meta→ilst)
// 视频：tkhd 宽高（16.16 定点）、mvhd 时长与创建时间
// 音频：ilst 的 ©nam/©ART/©alb/©day/trkn/©gen/©lyr、covr 封面
//
// 写入：ilst 内的 ilst 数据是"原子 box"，替换需要重建整个 ilst 及其父级
// meta/udta/moov 长度，因此采用"重建 moov → 重排文件"的策略：
//   1. 读出 moov
//   2. 在 moov 内替换/新增 ilst 条目，重算各级长度
//   3. 写临时文件：ftyp + 新 moov + mdat
//   4. 原子改名
package main

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"strings"
)

// mp4Box 一个 box
type mp4Box struct {
	Type    string
	Offset  int64 // 数据起始
	Size    int64 // 含头部的总长
	Payload []byte
}

// mp4ReadBox 读一个 box 头
func mp4ReadBox(f *os.File, off int64) (mp4Box, error) {
	var b mp4Box
	hdr := make([]byte, 8)
	if _, err := f.ReadAt(hdr, off); err != nil {
		return b, err
	}
	size := int64(beUint32(hdr[0:4]))
	b.Type = string(hdr[4:8])
	b.Offset = off + 8
	switch size {
	case 0:
		// 到文件末尾
		fi, err := f.Stat()
		if err != nil {
			return b, err
		}
		size = fi.Size() - off
	case 1:
		// 64 位长度
		ext := make([]byte, 8)
		if _, err := f.ReadAt(ext, off+8); err != nil {
			return b, err
		}
		size = int64(binary.BigEndian.Uint64(ext))
		b.Offset = off + 16
	}
	if size < 8 {
		return b, errors.New("box 尺寸异常")
	}
	b.Size = size
	return b, nil
}

// mp4FindBox 在给定区间内查找顶层 box
func mp4FindBox(f *os.File, start, end int64, typ string) (mp4Box, bool) {
	off := start
	for off < end {
		b, err := mp4ReadBox(f, off)
		if err != nil || b.Size <= 0 {
			return mp4Box{}, false
		}
		if b.Type == typ {
			return b, true
		}
		off += b.Size
	}
	return mp4Box{}, false
}

// mp4FindPath 逐层查找 box（路径如 ["moov","udta","meta","ilst"]）
func mp4FindPath(f *os.File, start, end int64, path ...string) (mp4Box, bool) {
	cur := mp4Box{Offset: start, Size: end - start}
	s, e := start, end
	for _, p := range path {
		b, ok := mp4FindBox(f, s, e, p)
		if !ok {
			return mp4Box{}, false
		}
		cur = b
		s, e = b.Offset, b.Offset+b.Size-8
	}
	return cur, true
}

// mp4ReadPayload 读取 box 的全部负载
func mp4ReadPayload(f *os.File, b mp4Box) ([]byte, error) {
	n := b.Size - (b.Offset - (b.Offset - 8))
	// 计算实际数据长度：b.Size 是总长（含 8 字节头，或 16 字节 64 位头）
	hdrLen := int64(8)
	if b.Offset >= 16 {
		// 可能是 64 位头，简单判断：读回长度字段
		hdr := make([]byte, 8)
		f.ReadAt(hdr, b.Offset-8)
		if beUint32(hdr[0:4]) == 1 {
			hdrLen = 16
		}
	}
	_ = n
	dataLen := b.Size - hdrLen
	if dataLen < 0 {
		return nil, errors.New("box 长度异常")
	}
	buf := make([]byte, dataLen)
	if _, err := f.ReadAt(buf, b.Offset); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return buf, nil
}

// readMP4 读取 MP4 元数据
func readMP4(path string) (MediaMeta, error) {
	var m MediaMeta
	f, err := os.Open(path)
	if err != nil {
		return m, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return m, err
	}
	total := fi.Size()

	moov, ok := mp4FindBox(f, 0, total, "moov")
	if !ok {
		return m, errors.New("未找到 moov box")
	}

	// mvhd：时长 + 创建时间
	if mvh, ok := mp4FindBox(f, moov.Offset, moov.Offset+moov.Size-8, "mvhd"); ok {
		if d, err := mp4ReadPayload(f, mvh); err == nil {
			m.Duration = parseMP4Duration(d)
		}
	}

	// trak→tkhd：宽高（视频轨）
	m.Width, m.Height = mp4VideoSize(f, moov)

	// udta→meta→ilst：标签
	if ilst, ok := mp4FindPath(f, moov.Offset, moov.Offset+moov.Size-8, "udta", "meta", "ilst"); ok {
		if data, err := mp4ReadPayload(f, ilst); err == nil {
			parseMP4Ilst(data, &m)
		}
	}
	// 部分文件 meta 是全 box（含 version/flags 4 字节），ilst 需再找
	if m.Title == "" {
		if meta, ok := mp4FindPath(f, moov.Offset, moov.Offset+moov.Size-8, "udta", "meta"); ok {
			if d, err := mp4ReadPayload(f, meta); err == nil && len(d) > 8 {
				// meta payload 去掉 4 字节 version/flags 后是子 box
				if ilst, ok := mp4FindBoxInMem(d[4:], "ilst"); ok {
					parseMP4Ilst(d[4+int(ilst.Offset):], &m)
				}
			}
		}
	}
	return m, nil
}

// mp4FindBoxInMem 在内存 buffer 中找 box
func mp4FindBoxInMem(data []byte, typ string) (mp4Box, bool) {
	off := int64(0)
	for off+8 <= int64(len(data)) {
		size := int64(beUint32(data[off : off+4]))
		bt := string(data[off+4 : off+8])
		hdr := int64(8)
		if size == 1 {
			if off+16 > int64(len(data)) {
				return mp4Box{}, false
			}
			size = int64(binary.BigEndian.Uint64(data[off+8 : off+16]))
			hdr = 16
		}
		if size < hdr || off+size > int64(len(data)) {
			return mp4Box{}, false
		}
		if bt == typ {
			return mp4Box{Type: bt, Offset: off + hdr, Size: size, Payload: data[off+hdr : off+size]}, true
		}
		off += size
	}
	return mp4Box{}, false
}

// parseMP4Duration 解析 mvhd 的时长（version 0: 32 位；version 1: 64 位）
func parseMP4Duration(d []byte) float64 {
	if len(d) < 4 {
		return 0
	}
	ver := d[0]
	if ver == 1 {
		if len(d) < 28 {
			return 0
		}
		ts := int64(beUint32(d[20:24]))
		dur := int64(binary.BigEndian.Uint64(d[24:32]))
		if ts > 0 {
			return float64(dur) / float64(ts)
		}
		return 0
	}
	if len(d) < 20 {
		return 0
	}
	ts := int64(beUint32(d[12:16]))
	dur := int64(beUint32(d[16:20]))
	if ts > 0 {
		return float64(dur) / float64(ts)
	}
	return 0
}

// mp4VideoSize 找视频轨的 tkhd 宽高
func mp4VideoSize(f *os.File, moov mp4Box) (int, int) {
	off := moov.Offset
	end := moov.Offset + moov.Size - 8
	for off < end {
		b, err := mp4ReadBox(f, off)
		if err != nil || b.Size <= 0 {
			break
		}
		if b.Type == "trak" {
			if tkhd, ok := mp4FindBox(f, b.Offset, b.Offset+b.Size-8, "tkhd"); ok {
				if d, err := mp4ReadPayload(f, tkhd); err == nil {
					w, h := parseTkhdSize(d)
					if w > 0 && h > 0 {
						return w, h
					}
				}
			}
		}
		off += b.Size
	}
	return 0, 0
}

// parseTkhdSize 解析 tkhd 尾部 16.16 定点宽高
func parseTkhdSize(d []byte) (int, int) {
	ver := d[0]
	var p int
	if ver == 1 {
		p = 4 + 8 + 8 + 4 + 4 + 8 // creation, modification, trackID, reserved, duration
	} else {
		p = 4 + 4 + 4 + 4 + 4 + 4
	}
	// reserved(8) + layer(2) + altgroup(2) + volume(2) + reserved(2) + matrix(36)
	p += 8 + 2 + 2 + 2 + 2 + 36
	if len(d) < p+8 {
		return 0, 0
	}
	w := int64(beUint32(d[p : p+4])) >> 16
	h := int64(beUint32(d[p+4 : p+8])) >> 16
	return int(w), int(h)
}

// parseMP4Ilst 解析 ilst 内的标签项
func parseMP4Ilst(data []byte, m *MediaMeta) {
	off := 0
	for off+8 <= len(data) {
		size := int(beUint32(data[off : off+4]))
		typ := string(data[off+4 : off+8])
		if size < 8 || off+size > len(data) {
			break
		}
		body := data[off+8 : off+size]
		val := mp4IlstValue(body)
		switch typ {
		case "\xa9nam":
			m.Title = val
		case "\xa9ART":
			m.Artist = val
		case "\xa9alb":
			m.Album = val
		case "\xa9day":
			m.Year = parseYear(val)
		case "\xa9gen":
			m.Genre = val
		case "\xa9cmt":
			m.Comment = val
		case "\xa9lyr":
			m.Lyrics = val
			m.HasLyric = val != ""
		case "trkn":
			// data: 4 字节 version/flags + 2 字节保留 + 2 字节音轨号 + 2 字节总数
			if len(body) >= 12 {
				payload := body[8:]
				if len(payload) >= 4 {
					m.Track = int(binary.BigEndian.Uint16(payload[2:4]))
					if len(payload) >= 6 {
						m.TrackTotal = int(binary.BigEndian.Uint16(payload[4:6]))
					}
				}
			}
		case "covr":
			m.HasCover = true
		}
		off += size
	}
}

// mp4IlstValue 取出 ilst 子项的文本值
func mp4IlstValue(body []byte) string {
	// body 是 data box：size(4) + 'data'(4) + version/flags(4) + locale(4) + payload
	if len(body) < 8 {
		return ""
	}
	if string(body[4:8]) != "data" {
		return ""
	}
	if len(body) < 16 {
		return ""
	}
	flags := beUint32(body[8:12])
	payload := body[16:]
	switch flags & 0x00FFFFFF {
	case 1: // UTF-8
		return trimNull(string(payload))
	case 2: // UTF-16
		return decodeUTF16LE(payload, false)
	case 0: // 二进制（如 trkn/covr）
		return ""
	default:
		return trimNull(string(payload))
	}
}

// ---------- 写 MP4 标签 ----------

// mp4BuildIlstItem 构造一个 ilst 条目（data box）
func mp4BuildIlstItem(typ string, payload []byte, dataType uint32) []byte {
	inner := make([]byte, 0, 16+len(payload))
	inner = append(inner, 0, 0, 0, 0) // size 占位
	inner = append(inner, []byte("data")...)
	var f [4]byte
	binary.BigEndian.PutUint32(f[:], dataType)
	inner = append(inner, f[:]...) // version/flags + locale
	inner = append(inner, f[:]...)
	inner = append(inner, payload...)
	binary.BigEndian.PutUint32(inner[0:4], uint32(len(inner)))
	out := make([]byte, 0, 8+len(inner))
	out = append(out, []byte(typ)...)
	var sz [4]byte
	binary.BigEndian.PutUint32(sz[:], uint32(len(inner)+8))
	out = append(out, sz[:]...)
	out = append(out, inner...)
	return out
}

// mp4TextItem 构造文本 ilst 条目（UTF-8）
func mp4TextItem(typ, text string) []byte {
	if text == "" {
		return nil
	}
	return mp4BuildIlstItem(typ, []byte(text), 1)
}

// writeMP4Tags 写回 MP4 标签（替换 ilst 中对应条目，保留其它条目）
func writeMP4Tags(path string, m MediaMeta, cover []byte) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	fi, err := src.Stat()
	if err != nil {
		return err
	}
	total := fi.Size()

	moov, ok := mp4FindBox(src, 0, total, "moov")
	if !ok {
		return errors.New("未找到 moov box")
	}
	moovData, err := mp4ReadPayload(src, moov)
	if err != nil {
		return err
	}

	// 定位 udta→meta→ilst
	metaBox, hasMeta := mp4FindBoxInMem(moovData, "meta")
	_ = metaBox
	var ilstData []byte
	if hasMeta {
		metaPayload := moovData[metaBox.Offset:]
		if len(metaPayload) > 4 {
			// meta 可能带 4 字节 version/flags
			if ib, ok := mp4FindBoxInMem(metaPayload[4:], "ilst"); ok {
				ilstData = metaPayload[4+int(ib.Offset):]
			} else if ib, ok := mp4FindBoxInMem(metaPayload, "ilst"); ok {
				ilstData = metaPayload[int(ib.Offset):]
			}
		}
	} else {
		// 没有 meta：新建
	}

	// 过滤要替换的旧条目，保留其它
	keep := []byte{}
	off := 0
	for off+8 <= len(ilstData) {
		size := int(beUint32(ilstData[off : off+4]))
		typ := string(ilstData[off+4 : off+8])
		if size < 8 || off+size > len(ilstData) {
			break
		}
		switch typ {
		case "\xa9nam", "\xa9ART", "\xa9alb", "\xa9day", "\xa9gen", "\xa9cmt", "\xa9lyr", "trkn", "covr":
			// 丢弃旧的（会重建）
		default:
			keep = append(keep, ilstData[off:off+size]...)
		}
		off += size
	}

	// 新条目
	newItems := keep
	if v := mp4TextItem("\xa9nam", m.Title); v != nil {
		newItems = append(newItems, v...)
	}
	if v := mp4TextItem("\xa9ART", m.Artist); v != nil {
		newItems = append(newItems, v...)
	}
	if v := mp4TextItem("\xa9alb", m.Album); v != nil {
		newItems = append(newItems, v...)
	}
	if m.Year > 0 {
		if v := mp4TextItem("\xa9day", itoa(m.Year)); v != nil {
			newItems = append(newItems, v...)
		}
	}
	if v := mp4TextItem("\xa9gen", m.Genre); v != nil {
		newItems = append(newItems, v...)
	}
	if v := mp4TextItem("\xa9lyr", m.Lyrics); v != nil {
		newItems = append(newItems, v...)
	}
	if m.Track > 0 {
		p := make([]byte, 8)
		binary.BigEndian.PutUint16(p[2:4], uint16(m.Track))
		if m.TrackTotal > 0 {
			binary.BigEndian.PutUint16(p[4:6], uint16(m.TrackTotal))
		}
		newItems = append(newItems, mp4BuildIlstItem("trkn", p, 0)...)
	}
	if cover != nil {
		newItems = append(newItems, mp4BuildIlstItem("covr", cover, 13)...)
	}

	// 重建 ilst box
	ilstBox := make([]byte, 0, 8+len(newItems))
	var sz [4]byte
	binary.BigEndian.PutUint32(sz[:], uint32(8+len(newItems)))
	ilstBox = append(ilstBox, sz[:]...)
	ilstBox = append(ilstBox, []byte("ilst")...)
	ilstBox = append(ilstBox, newItems...)

	// 重建 meta box（full box：version/flags 4 字节 + 子 box）
	metaPayload := make([]byte, 0, 4+len(ilstBox))
	metaPayload = append(metaPayload, 0, 0, 0, 0)
	metaPayload = append(metaPayload, ilstBox...)
	metaBoxBytes := make([]byte, 0, 8+len(metaPayload))
	binary.BigEndian.PutUint32(sz[:], uint32(8+len(metaPayload)))
	metaBoxBytes = append(metaBoxBytes, sz[:]...)
	metaBoxBytes = append(metaBoxBytes, []byte("meta")...)
	metaBoxBytes = append(metaBoxBytes, metaPayload...)

	// 重建 moov：把旧 udta 换成新的（无 udta 则追加）
	newMoov := rebuildMoov(moovData, metaBoxBytes)
	return mp4RewriteFile(src, path, newMoov, moov, total)
}

// rebuildMoov 重建 moov 内容：替换或插入 udta→meta
func rebuildMoov(moovData, newMeta []byte) []byte {
	out := make([]byte, 0, len(moovData)+len(newMeta))
	off := 0
	inserted := false
	for off+8 <= len(moovData) {
		size := int(beUint32(moovData[off : off+4]))
		typ := string(moovData[off+4 : off+8])
		if size < 8 || off+size > len(moovData) {
			break
		}
		if typ == "udta" {
			// 替换 udta 内的 meta
			udta := moovData[off+8 : off+size]
			newUdta := rebuildUdta(udta, newMeta)
			out = append(out, udtaBox(newUdta)...)
			inserted = true
		} else {
			out = append(out, moovData[off:off+size]...)
		}
		off += size
	}
	if !inserted {
		// 没有 udta：新建 udta→meta 追加到 moov 末尾
		udta := append([]byte{}, newMeta...)
		out = append(out, udtaBox(udta)...)
	}
	return out
}

// rebuildUdta 替换 udta 内的 meta，保留其它子 box
func rebuildUdta(udta, newMeta []byte) []byte {
	out := make([]byte, 0, len(udta)+len(newMeta))
	off := 0
	found := false
	for off+8 <= len(udta) {
		size := int(beUint32(udta[off : off+4]))
		typ := string(udta[off+4 : off+8])
		if size < 8 || off+size > len(udta) {
			break
		}
		if typ == "meta" {
			out = append(out, newMeta...)
			found = true
		} else {
			out = append(out, udta[off:off+size]...)
		}
		off += size
	}
	if !found {
		out = append(out, newMeta...)
	}
	return out
}

// udtaBox 把内容包成 udta box
func udtaBox(content []byte) []byte {
	var sz [4]byte
	binary.BigEndian.PutUint32(sz[:], uint32(8+len(content)))
	out := make([]byte, 0, 8+len(content))
	out = append(out, sz[:]...)
	out = append(out, []byte("udta")...)
	out = append(out, content...)
	return out
}

// mp4RewriteFile 重写文件：ftyp + 新 moov + 其余 box（原样复制）
func mp4RewriteFile(src *os.File, path string, newMoov []byte, moov mp4Box, total int64) error {
	tmp := mediaTempPath(path)
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	okAll := false
	defer func() {
		out.Close()
		if !okAll {
			os.Remove(tmp)
		}
	}()

	// moov box（带头）
	var sz [4]byte
	binary.BigEndian.PutUint32(sz[:], uint32(8+len(newMoov)))
	if _, err := out.Write(sz[:]); err != nil {
		return err
	}
	if _, err := out.Write([]byte("moov")); err != nil {
		return err
	}
	if _, err := out.Write(newMoov); err != nil {
		return err
	}

	// 复制 moov 之前与之后的原始字节
	if moov.Offset-8 > 0 {
		if _, err := src.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.CopyN(out, src, moov.Offset-8); err != nil {
			return err
		}
	}
	// 复制 moov 之后的数据
	after := moov.Offset + moov.Size - 8
	if after < total {
		if _, err := src.Seek(after, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.Copy(out, src); err != nil {
			return err
		}
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if fi, err := os.Stat(path); err == nil {
		os.Chtimes(tmp, fi.ModTime(), fi.ModTime())
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	okAll = true
	return nil
}

// itoa 简易整数转字符串（避免额外 import 冲突时可用）
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// mp4StripTagsOf 取 MP4 去掉 udta 的信息（调试用）
func mp4StripTagsOf(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, _ := f.Stat()
	moov, ok := mp4FindBox(f, 0, fi.Size(), "moov")
	if !ok {
		return ""
	}
	if _, ok := mp4FindPath(f, moov.Offset, moov.Offset+moov.Size-8, "udta"); ok {
		return "有 udta"
	}
	return "无 udta"
}

// mp4HasTag 判断 MP4 是否已有 udta
func mp4HasTag(path string) bool { return mp4StripTagsOf(path) == "有 udta" }

// mp4TitleQuick 快速取标题（用于列表展示）
func mp4TitleQuick(path string) string {
	m, err := readMP4(path)
	if err != nil {
		return ""
	}
	return m.Title
}

// mp4JoinTitles 合并标题片段
func mp4JoinTitles(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " ")
}
