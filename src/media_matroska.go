// Matroska / WebM 解析（EBML 格式）
//
// 关键元素：
//   Segment → Info → Title(0x7BA9)、Duration(0x4489, 时间刻度)
//   Segment → Tracks → TrackEntry → Video(0xE0) → PixelWidth(0xB0)/PixelHeight(0xBA)
//   Segment → Tags → Tag → SimpleTag → TagName(0x45A3)/TagString(0x4487)
//
// EBML 变长整数：首字节前导 1 表示长度，值含标记位需剥离。
package main

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"strings"
)

// ebmlReadVInt 读 EBML 变长整数：返回值与是否未知尺寸
func ebmlReadVInt(d []byte, p int) (val uint64, size int, unknown bool) {
	if p >= len(d) {
		return 0, 0, false
	}
	first := d[p]
	if first == 0 {
		return 0, 0, false
	}
	length := 1
	mask := byte(0x80)
	for length <= 8 && first&mask == 0 {
		length++
		mask >>= 1
	}
	if length > 8 || p+length > len(d) {
		return 0, 0, false
	}
	v := uint64(first & (mask - 1))
	for i := 1; i < length; i++ {
		v = v<<8 | uint64(d[p+i])
	}
	unknown = v == (uint64(1)<<(7*length) - 1)
	return v, length, unknown
}

// ebmlReadID 读元素 ID（保留标记位）
func ebmlReadID(d []byte, p int) (id uint64, size int) {
	if p >= len(d) {
		return 0, 0
	}
	first := d[p]
	length := 1
	mask := byte(0x80)
	for length <= 4 && first&mask == 0 {
		length++
		mask >>= 1
	}
	if p+length > len(d) {
		return 0, 0
	}
	var v uint64
	for i := 0; i < length; i++ {
		v = v<<8 | uint64(d[p+i])
	}
	return v, length
}

// readMatroska 读取 MKV/WebM 元数据
func readMatroska(path string) (MediaMeta, error) {
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
	// 读前若干 MB 足够覆盖 Info/Tracks/Tags（都在文件头附近）
	const headSize = 4 << 20
	size := fi.Size()
	if size > headSize {
		size = headSize
	}
	data := make([]byte, size)
	if _, err := f.ReadAt(data, 0); err != nil && !errors.Is(err, io.EOF) {
		return m, err
	}
	data = data[:size]

	// 跳过 EBML 头，找 Segment
	p := 0
	// EBML header
	if _, n := ebmlReadID(data, p); n > 0 {
		if _, sn, _ := ebmlReadVInt(data, p+n); sn > 0 {
			p += n + sn
		}
	}
	// 找 Segment
	segID := uint64(0x18538067)
	segStart, segEnd := -1, len(data)
	for p < len(data) {
		id, n := ebmlReadID(data, p)
		if n == 0 {
			break
		}
		sz, sn, unknown := ebmlReadVInt(data, p+n)
		bodyStart := p + n + sn
		if id == segID {
			segStart = bodyStart
			if !unknown && bodyStart+int(sz) <= len(data) {
				segEnd = bodyStart + int(sz)
			}
			break
		}
		if unknown {
			break
		}
		p = bodyStart + int(sz)
	}
	if segStart < 0 {
		return m, errors.New("未找到 Segment")
	}

	// 遍历 Segment 内的顶级元素
	q := segStart
	for q < segEnd {
		id, n := ebmlReadID(data, q)
		if n == 0 {
			break
		}
		sz, sn, unknown := ebmlReadVInt(data, q+n)
		bodyStart := q + n + sn
		if !unknown && bodyStart+int(sz) > segEnd {
			bodyStart = segStart
		}
		switch id {
		case 0x1549A966: // Info
			parseMkvInfo(data, bodyStart, minInt(bodyStart+int(sz), segEnd), &m)
		case 0x1654AE6B: // Tracks
			parseMkvTracks(data, bodyStart, minInt(bodyStart+int(sz), segEnd), &m)
		case 0x1254C367: // Tags
			parseMkvTags(data, bodyStart, minInt(bodyStart+int(sz), segEnd), &m)
		}
		if unknown {
			break
		}
		q = bodyStart + int(sz)
	}
	return m, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// parseMkvInfo 解析 Info 元素
func parseMkvInfo(d []byte, start, end int, m *MediaMeta) {
	p := start
	for p < end {
		id, n := ebmlReadID(d, p)
		if n == 0 {
			break
		}
		sz, sn, unknown := ebmlReadVInt(d, p+n)
		body := p + n + sn
		if unknown {
			break
		}
		switch id {
		case 0x7BA9: // Title
			if body+int(sz) <= len(d) {
				m.Title = trimNull(string(d[body : body+int(sz)]))
			}
		case 0x4489: // Duration（浮点，单位毫秒）
			if body+int(sz) <= len(d) && sz >= 4 {
				var v float64
				switch sz {
				case 4:
					v = float64(math.Float32frombits(beUint32(d[body : body+4])))
				case 8:
					v = math.Float64frombits(binary.BigEndian.Uint64(d[body : body+8]))
				}
				m.Duration = v / 1000.0
			}
		}
		p = body + int(sz)
	}
}

// parseMkvTracks 解析 Tracks → TrackEntry → Video
func parseMkvTracks(d []byte, start, end int, m *MediaMeta) {
	p := start
	for p < end {
		id, n := ebmlReadID(d, p)
		if n == 0 {
			break
		}
		sz, sn, unknown := ebmlReadVInt(d, p+n)
		body := p + n + sn
		if unknown {
			break
		}
		if id == 0xAE { // TrackEntry
			parseMkvTrackEntry(d, body, body+int(sz), m)
		}
		p = body + int(sz)
	}
}

// parseMkvTrackEntry 找 Video 轨的宽高
func parseMkvTrackEntry(d []byte, start, end int, m *MediaMeta) {
	p := start
	for p < end {
		id, n := ebmlReadID(d, p)
		if n == 0 {
			break
		}
		sz, sn, unknown := ebmlReadVInt(d, p+n)
		body := p + n + sn
		if unknown {
			break
		}
		if id == 0xE0 { // Video
			w, h := parseMkvVideo(d, body, body+int(sz))
			if w > 0 && h > 0 {
				m.Width, m.Height = w, h
				return
			}
		}
		p = body + int(sz)
	}
}

// parseMkvVideo 解析 Video 元素里的宽高
func parseMkvVideo(d []byte, start, end int) (int, int) {
	w, h := 0, 0
	p := start
	for p < end {
		id, n := ebmlReadID(d, p)
		if n == 0 {
			break
		}
		sz, sn, unknown := ebmlReadVInt(d, p+n)
		body := p + n + sn
		if unknown {
			break
		}
		switch id {
		case 0xB0: // PixelWidth
			if body+int(sz) <= len(d) {
				w = int(ebmlUint(d[body : body+int(sz)]))
			}
		case 0xBA: // PixelHeight
			if body+int(sz) <= len(d) {
				h = int(ebmlUint(d[body : body+int(sz)]))
			}
		}
		p = body + int(sz)
	}
	return w, h
}

// parseMkvTags 解析 Tags → Tag → SimpleTag
func parseMkvTags(d []byte, start, end int, m *MediaMeta) {
	p := start
	for p < end {
		id, n := ebmlReadID(d, p)
		if n == 0 {
			break
		}
		sz, sn, unknown := ebmlReadVInt(d, p+n)
		body := p + n + sn
		if unknown {
			break
		}
		if id == 0x7373 { // Tag
			name, value := parseMkvTag(d, body, body+int(sz))
			switch strings.ToUpper(name) {
			case "TITLE":
				if m.Title == "" {
					m.Title = value
				}
			case "ARTIST", "PERFORMER":
				if m.Artist == "" {
					m.Artist = value
				}
			case "ALBUM":
				if m.Album == "" {
					m.Album = value
				}
			case "DATE", "YEAR":
				if m.Year == 0 {
					m.Year = parseYear(value)
				}
			case "COMMENT":
				if m.Comment == "" {
					m.Comment = value
				}
			}
		}
		p = body + int(sz)
	}
}

// parseMkvTag 解析单个 Tag 的第一个 SimpleTag
func parseMkvTag(d []byte, start, end int) (string, string) {
	name, value := "", ""
	p := start
	for p < end {
		id, n := ebmlReadID(d, p)
		if n == 0 {
			break
		}
		sz, sn, unknown := ebmlReadVInt(d, p+n)
		body := p + n + sn
		if unknown {
			break
		}
		if id == 0x67C8 { // SimpleTag
			n2, v2 := parseMkvSimpleTag(d, body, body+int(sz))
			if n2 != "" {
				name, value = n2, v2
			}
		}
		p = body + int(sz)
	}
	return name, value
}

// parseMkvSimpleTag 解析 SimpleTag 的 TagName/TagString
func parseMkvSimpleTag(d []byte, start, end int) (string, string) {
	name, value := "", ""
	p := start
	for p < end {
		id, n := ebmlReadID(d, p)
		if n == 0 {
			break
		}
		sz, sn, unknown := ebmlReadVInt(d, p+n)
		body := p + n + sn
		if unknown {
			break
		}
		switch id {
		case 0x45A3: // TagName
			if body+int(sz) <= len(d) {
				name = trimNull(string(d[body : body+int(sz)]))
			}
		case 0x4487: // TagString
			if body+int(sz) <= len(d) {
				value = trimNull(string(d[body : body+int(sz)]))
			}
		}
		p = body + int(sz)
	}
	return name, value
}

// ebmlUint 把字节数组转成无符号整数
func ebmlUint(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

// ---------- WAV ----------

// readWAV 读取 WAV 的基本元数据
func readWAV(path string) (MediaMeta, error) {
	var m MediaMeta
	f, err := os.Open(path)
	if err != nil {
		return m, err
	}
	defer f.Close()
	head := make([]byte, 12)
	if _, err := io.ReadFull(f, head); err != nil {
		return m, err
	}
	if string(head[:4]) != "RIFF" || string(head[8:12]) != "WAVE" {
		return m, errors.New("不是 WAV 文件")
	}
	// 遍历 chunk 找 LIST(INFO) 与 id3
	off := int64(12)
	for {
		ch := make([]byte, 8)
		if _, err := f.ReadAt(ch, off); err != nil {
			break
		}
		id := string(ch[:4])
		size := int64(leUint32(ch[4:8]))
		if size < 0 || size > 100<<20 {
			break
		}
		switch id {
		case "LIST":
			data := make([]byte, size)
			if _, err := f.ReadAt(data, off+8); err == nil && len(data) > 4 && string(data[:4]) == "INFO" {
				parseWavInfo(data[4:], &m)
			}
		case "id3 ", "ID3 ":
			data := make([]byte, size)
			if _, err := f.ReadAt(data, off+8); err == nil {
				frames := parseID3Frames(data, 3)
				tt := &id3Tag{Frames: frames}
				if m.Title == "" {
					m.Title = tt.id3FrameValue("TIT2")
				}
				if m.Artist == "" {
					m.Artist = tt.id3FrameValue("TPE1")
				}
				if m.Album == "" {
					m.Album = tt.id3FrameValue("TALB")
				}
				if m.Year == 0 {
					m.Year = parseYear(tt.id3FrameValueAny("TDRC", "TYER"))
				}
			}
		case "fmt ":
			data := make([]byte, size)
			if _, err := f.ReadAt(data, off+8); err == nil && len(data) >= 16 {
				byteRate := int(leUint32(data[8:12]))
				dataSize := int(leUint32(data[4:8]))
				if byteRate > 0 && dataSize > 0 {
					m.Duration = float64(dataSize) / float64(byteRate)
				}
			}
		}
		off += 8 + size
		if size%2 == 1 {
			off++
		}
	}
	return m, nil
}

// parseWavInfo 解析 LIST/INFO 子块
func parseWavInfo(d []byte, m *MediaMeta) {
	p := 0
	for p+8 <= len(d) {
		id := string(d[p : p+4])
		size := int(leUint32(d[p+4 : p+8]))
		if size < 0 || p+8+size > len(d) {
			break
		}
		val := trimNull(string(d[p+8 : p+8+size]))
		switch strings.ToUpper(id) {
		case "INAM":
			if m.Title == "" {
				m.Title = val
			}
		case "IART":
			if m.Artist == "" {
				m.Artist = val
			}
		case "IPRD":
			if m.Album == "" {
				m.Album = val
			}
		case "ICRD":
			if m.Year == 0 {
				m.Year = parseYear(val)
			}
		case "IGNR":
			if m.Genre == "" {
				m.Genre = val
			}
		}
		p += 8 + size
		if size%2 == 1 {
			p++
		}
	}
}
