package main

// WMA / ASF 标签读写（纯标准库）。
//
// 为什么需要单独一套：WMA 既不是 ID3 也不是 Vorbis/MP4，它是微软 ASF 容器——
// 元数据放在 Header Object 的两个子对象里：
//   - Content Description Object：标题 / 艺术家 / 版权 / 描述 / 分级（UTF-16LE）
//   - Extended Content Description Object：WM/AlbumTitle、WM/Year、
//     WM/TrackNumber、WM/Genre 等描述符
//
// 写入策略：
//   1. 解析整个 Header Object 的子对象，丢掉旧的 CD/ECD/Padding；
//   2. 生成新的 CD/ECD，重算 Header Object 长度；
//   3. 头部长度变化会挪动数据区，所以同步修正 File Properties 里的「文件大小」，
//      再整体重写（先写临时文件，成功才原子改名，中途失败不破坏原文件）。
//
// 未涉及的内容（Header Extension 里的 WM/Picture 等）原样保留。

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"unicode/utf16"
)

// ASF 对象 GUID（按文件中的小端字节序书写）
var (
	asfGUIDHeader    = [16]byte{0x30, 0x26, 0xB2, 0x75, 0x8E, 0x66, 0xCF, 0x11, 0xA6, 0xD9, 0x00, 0xAA, 0x00, 0x62, 0xCE, 0x6C}
	asfGUIDFileProps = [16]byte{0xA1, 0xDC, 0xAB, 0x8C, 0x47, 0xA9, 0xCF, 0x11, 0x8E, 0xE4, 0x00, 0xC0, 0x0C, 0x20, 0x53, 0x65}
	asfGUIDContent   = [16]byte{0x33, 0x26, 0xB2, 0x75, 0x8E, 0x66, 0xCF, 0x11, 0xA6, 0xD9, 0x00, 0xAA, 0x00, 0x62, 0xCE, 0x6C}
	asfGUIDExtCont   = [16]byte{0x40, 0xA4, 0xD0, 0xD2, 0x07, 0xE3, 0xD2, 0x11, 0x97, 0xF0, 0x00, 0xA0, 0xC9, 0x5E, 0xA8, 0x50}
	asfGUIDPadding   = [16]byte{0x74, 0xD4, 0x06, 0x18, 0xDF, 0xCA, 0x45, 0x09, 0xA4, 0xBA, 0x9A, 0xAB, 0xCB, 0x96, 0xAA, 0xE8}
	asfGUIDHeaderExt = [16]byte{0xB5, 0x03, 0xBF, 0x5F, 0x2E, 0xA9, 0xCF, 0x11, 0x8E, 0xE3, 0x00, 0xC0, 0x0C, 0x20, 0x53, 0x65}
	asfGUIDData      = [16]byte{0x36, 0x26, 0xB2, 0x75, 0x8E, 0x66, 0xCF, 0x11, 0xA6, 0xD9, 0x00, 0xAA, 0x00, 0x62, 0xCE, 0x6C}
)

const asfHeaderFixed = 30 // GUID(16) + Size(8) + 对象数(4) + 保留(2)

// isASF 判断文件头是否为 ASF 容器
func isASF(head []byte) bool {
	return len(head) >= 16 && bytes.Equal(head[:16], asfGUIDHeader[:])
}

// asfUTF16 编码为 UTF-16LE（不带 BOM，不带结尾 0）
func asfUTF16(s string) []byte {
	out := make([]byte, 0, len(s)*2)
	for _, r := range s {
		if r > 0xFFFF {
			r1, r2 := utf16.EncodeRune(r)
			out = append(out, byte(r1), byte(r1>>8), byte(r2), byte(r2>>8))
			continue
		}
		out = append(out, byte(r), byte(r>>8))
	}
	return out
}

// asfReadUTF16 解码 UTF-16LE 并去掉结尾 0
func asfReadUTF16(b []byte) string {
	return strings.TrimRight(decodeUTF16LE(b, false), "\x00")
}

// asfObject 一个 Header 子对象
type asfObject struct {
	GUID [16]byte
	Data []byte // 完整对象（含 24 字节头）
}

// asfReadHeader 读取 Header Object 及其子对象
func asfReadHeader(f *os.File) (subs []asfObject, headerSize int64, err error) {
	head := make([]byte, asfHeaderFixed)
	if _, err = f.ReadAt(head, 0); err != nil {
		return nil, 0, err
	}
	if !bytes.Equal(head[:16], asfGUIDHeader[:]) {
		return nil, 0, errors.New("不是 ASF 文件")
	}
	headerSize = int64(binary.LittleEndian.Uint64(head[16:24]))
	if headerSize < asfHeaderFixed {
		return nil, 0, errors.New("ASF 头部长度非法")
	}
	body := make([]byte, headerSize-asfHeaderFixed)
	if _, err = f.ReadAt(body, asfHeaderFixed); err != nil && err != io.EOF {
		return nil, 0, err
	}
	// 子对象：[GUID 16][Size 8][Payload]
	p := 0
	for p+24 <= len(body) {
		var g [16]byte
		copy(g[:], body[p:p+16])
		size := int64(binary.LittleEndian.Uint64(body[p+16 : p+24]))
		if size < 24 || int64(p)+size > int64(len(body)) {
			break // 长度异常，停止解析（保留已读到的）
		}
		subs = append(subs, asfObject{GUID: g, Data: body[p : p+int(size)]})
		p += int(size)
	}
	return subs, headerSize, nil
}

// readASF 读取 WMA/ASF 元数据
func readASF(path string) (MediaMeta, error) {
	var m MediaMeta
	f, err := os.Open(path)
	if err != nil {
		return m, err
	}
	defer f.Close()
	subs, _, err := asfReadHeader(f)
	if err != nil {
		return m, err
	}
	for _, o := range subs {
		switch o.GUID {
		case asfGUIDContent:
			if len(o.Data) < 24+10 {
				continue
			}
			pl := o.Data[24:]
			l := [5]int{}
			for i := 0; i < 5; i++ {
				l[i] = int(binary.LittleEndian.Uint16(pl[i*2 : i*2+2]))
			}
			p := 10
			take := func(n int) string {
				if n <= 0 || p+n > len(pl) {
					return ""
				}
				s := asfReadUTF16(pl[p : p+n])
				p += n
				return s
			}
			m.Title = take(l[0])
			m.Artist = take(l[1])
			// 版权 / 描述 / 分级不参与整理，跳过
		case asfGUIDExtCont:
			if len(o.Data) < 24+2 {
				continue
			}
			pl := o.Data[24:]
			cnt := int(binary.LittleEndian.Uint16(pl[0:2]))
			p := 2
			for i := 0; i < cnt && p+2 <= len(pl); i++ {
				nameLen := int(binary.LittleEndian.Uint16(pl[p : p+2]))
				p += 2
				if p+nameLen+4 > len(pl) {
					break
				}
				name := strings.ToUpper(asfReadUTF16(pl[p : p+nameLen]))
				p += nameLen
				vtype := binary.LittleEndian.Uint16(pl[p : p+2])
				vlen := int(binary.LittleEndian.Uint16(pl[p+2 : p+4]))
				p += 4
				if p+vlen > len(pl) {
					break
				}
				val := pl[p : p+vlen]
				p += vlen
				var text string
				switch vtype {
				case 0: // Unicode 字符串
					text = strings.TrimRight(asfReadUTF16(val), "\x00")
				case 3: // DWORD
					if len(val) >= 4 {
						text = strconv.FormatUint(uint64(binary.LittleEndian.Uint32(val)), 10)
					}
				case 5: // WORD
					if len(val) >= 2 {
						text = strconv.FormatUint(uint64(binary.LittleEndian.Uint16(val)), 10)
					}
				case 4: // QWORD
					if len(val) >= 8 {
						text = strconv.FormatUint(binary.LittleEndian.Uint64(val), 10)
					}
				}
				switch name {
				case "WM/ALBUMTITLE", "WM/ALBUM":
					m.Album = text
				case "WM/YEAR", "WM/ORIGINALRELEASEYEAR":
					if y := parseYear(text); y > 0 {
						m.Year = y
					}
				case "WM/TRACKNUMBER", "WM/TRACK":
					if n, tot := parseTrack(text); n > 0 {
						m.Track, m.TrackTotal = n, tot
					}
				case "WM/GENRE", "WM/GENREID":
					if m.Genre == "" {
						m.Genre = text
					}
				}
			}
		case asfGUIDFileProps:
			// 时长 = PlayDuration(100ns) - Preroll(ms)
			if len(o.Data) >= 24+64 {
				pl := o.Data[24:]
				play := binary.LittleEndian.Uint64(pl[40:48])
				pre := binary.LittleEndian.Uint64(pl[56:64])
				if play > 0 {
					sec := float64(play)/1e7 - float64(pre)/1000
					if sec > 0 {
						m.Duration = sec
					}
				}
			}
		}
	}
	return m, nil
}

// ---------- 写入 ----------

// asfBuildContentDesc 构造 Content Description Object（标题/艺术家）
func asfBuildContentDesc(m MediaMeta) []byte {
	title := append(asfUTF16(m.Title), 0, 0)
	artist := append(asfUTF16(m.Artist), 0, 0)
	if m.Title == "" {
		title = nil
	}
	if m.Artist == "" {
		artist = nil
	}
	// 版权 / 描述 / 分级留空（不覆盖用户原有内容是不保留旧对象的代价，故置空更安全）
	size := 24 + 10 + len(title) + len(artist)
	out := make([]byte, 0, size)
	out = append(out, asfGUIDContent[:]...)
	sz := make([]byte, 8)
	binary.LittleEndian.PutUint64(sz, uint64(size))
	out = append(out, sz...)
	for _, n := range []int{len(title), len(artist), 0, 0, 0} {
		w := make([]byte, 2)
		binary.LittleEndian.PutUint16(w, uint16(n))
		out = append(out, w...)
	}
	out = append(out, title...)
	out = append(out, artist...)
	return out
}

// asfBuildExtContentDesc 构造 Extended Content Description Object
func asfBuildExtContentDesc(m MediaMeta) []byte {
	type kv struct {
		name  string
		value string
		dword bool
	}
	var items []kv
	if m.Album != "" {
		items = append(items, kv{"WM/AlbumTitle", m.Album, false})
	}
	if m.Year > 0 {
		items = append(items, kv{"WM/Year", strconv.Itoa(m.Year), false})
	}
	if m.Track > 0 {
		// WM/TrackNumber 用 DWORD：Windows 媒体库按数值排序，写成字符串会排错
		items = append(items, kv{"WM/TrackNumber", strconv.Itoa(m.Track), true})
	}
	if m.Genre != "" {
		items = append(items, kv{"WM/Genre", m.Genre, false})
	}
	payload := make([]byte, 2)
	binary.LittleEndian.PutUint16(payload, uint16(len(items)))
	for _, it := range items {
		name := asfUTF16(it.name)
		w := make([]byte, 2)
		binary.LittleEndian.PutUint16(w, uint16(len(name)))
		payload = append(payload, w...)
		payload = append(payload, name...)
		if it.dword {
			binary.LittleEndian.PutUint16(w, 3) // DWORD
			payload = append(payload, w...)
			binary.LittleEndian.PutUint16(w, 4)
			payload = append(payload, w...)
			d := make([]byte, 4)
			n, _ := strconv.ParseUint(it.value, 10, 32)
			binary.LittleEndian.PutUint32(d, uint32(n))
			payload = append(payload, d...)
		} else {
			val := append(asfUTF16(it.value), 0, 0)
			binary.LittleEndian.PutUint16(w, 0) // Unicode
			payload = append(payload, w...)
			binary.LittleEndian.PutUint16(w, uint16(len(val)))
			payload = append(payload, w...)
			payload = append(payload, val...)
		}
	}
	size := 24 + len(payload)
	out := make([]byte, 0, size)
	out = append(out, asfGUIDExtCont[:]...)
	sz := make([]byte, 8)
	binary.LittleEndian.PutUint64(sz, uint64(size))
	out = append(out, sz...)
	out = append(out, payload...)
	return out
}

// writeASFTags 重写 ASF 头部标签（WMA/WMV）
func writeASFTags(path string, m MediaMeta, cover []byte) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	fi, err := src.Stat()
	if err != nil {
		return err
	}
	subs, oldHeaderSize, err := asfReadHeader(src)
	if err != nil {
		return err
	}

	content := asfBuildContentDesc(m)
	ext := asfBuildExtContentDesc(m)
	// 未写入封面的原因：WM/Picture 位于 Header Extension 的 Metadata Object 内，
	// 重建它风险高；这里保留原对象，封面通过原样拷贝不丢失。

	out := make([][]byte, 0, len(subs)+2)
	hasProps := false
	insertAt := -1
	for _, o := range subs {
		switch o.GUID {
		case asfGUIDContent, asfGUIDExtCont, asfGUIDPadding:
			continue // 由新对象替换
		case asfGUIDFileProps:
			hasProps = true
		case asfGUIDHeaderExt:
			// 内容对象按规范放在 Header Extension 之前（严格解析器对此敏感）
			insertAt = len(out)
		}
		out = append(out, o.Data)
	}
	if !hasProps {
		return errors.New("ASF 头部缺少 File Properties 对象，放弃写入")
	}
	if insertAt >= 0 {
		tail := append([][]byte{}, out[insertAt:]...)
		out = append(append(out[:insertAt:insertAt], content, ext), tail...)
	} else {
		out = append(out, content, ext)
	}
	newHeaderSize := int64(asfHeaderFixed)
	for _, b := range out {
		newHeaderSize += int64(len(b))
	}
	delta := newHeaderSize - oldHeaderSize
	newTotal := fi.Size() + delta

	// 修正 Header Object 头：长度 / 对象数
	head := make([]byte, asfHeaderFixed)
	copy(head, asfGUIDHeader[:])
	binary.LittleEndian.PutUint64(head[16:24], uint64(newHeaderSize))
	binary.LittleEndian.PutUint32(head[24:28], uint32(len(out)))
	head[28] = 1
	head[29] = 2

	// 修正 File Properties 中的「文件大小」（payload 偏移 16）
	for i := range out {
		if bytes.Equal(out[i][:16], asfGUIDFileProps[:]) && len(out[i]) >= 24+24 {
			binary.LittleEndian.PutUint64(out[i][24+16:24+24], uint64(newTotal))
		}
	}

	tmp := mediaTempPath(path)
	dst, err := os.Create(tmp)
	if err != nil {
		return err
	}
	okAll := false
	defer func() {
		dst.Close()
		if !okAll {
			os.Remove(tmp)
		}
	}()
	if _, err := dst.Write(head); err != nil {
		return err
	}
	for _, b := range out {
		if _, err := dst.Write(b); err != nil {
			return err
		}
	}
	// 拷贝数据区（从旧头部末尾到 EOF）
	if _, err := src.Seek(oldHeaderSize, io.SeekStart); err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	if err := dst.Sync(); err != nil {
		return err
	}
	dst.Close()
	os.Chtimes(tmp, fi.ModTime(), fi.ModTime())
	// 先关源文件再改名：Windows 不允许覆盖打开中的文件（Access denied）
	src.Close()
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	okAll = true
	return nil
}
