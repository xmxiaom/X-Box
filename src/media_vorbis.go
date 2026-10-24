// Vorbis Comment 解析与写入（FLAC / OGG / Opus）
//
// Vorbis Comment 格式：vendor_length(LE32) + vendor + count(LE32) +
//   每条：length(LE32) + "KEY=value"（UTF-8）
//
// FLAC 布局："fLaC" + metadata block（每个：1 字节标志 + 3 字节长度 + 数据）
//   VORBIS_COMMENT 块类型 = 4
//   PICTURE 块类型 = 6（封面）
//
// OGG 布局：分页，每页含 packet；packet 里是 Vorbis 头（"\x03vorbis"）
//   或 Opus 头（"OpusTags"）
package main

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
)

// vorbisTags 解析出的标签集合（保留原始顺序便于回写）
type vorbisTags struct {
	Vendor string
	Items  []vorbisItem // KEY=VALUE 原始串
	Pics   [][]byte     // 封面数据
}

type vorbisItem struct {
	Key   string
	Value string
}

// get 取第一个匹配键的值（不区分大小写）
func (t *vorbisTags) get(key string) string {
	k := strings.ToLower(key)
	for _, it := range t.Items {
		if strings.ToLower(it.Key) == k {
			return it.Value
		}
	}
	return ""
}

// set 设置键值（存在则替换，否则追加）
func (t *vorbisTags) set(key, val string) {
	if val == "" {
		return
	}
	k := strings.ToLower(key)
	for i := range t.Items {
		if strings.ToLower(t.Items[i].Key) == k {
			t.Items[i].Value = val
			return
		}
	}
	t.Items = append(t.Items, vorbisItem{Key: key, Value: val})
}

// remove 删除键
func (t *vorbisTags) remove(key string) {
	k := strings.ToLower(key)
	out := t.Items[:0]
	for _, it := range t.Items {
		if strings.ToLower(it.Key) != k {
			out = append(out, it)
		}
	}
	t.Items = out
}

// parseVorbisCommentBlock 解析 Vorbis Comment 数据块
func parseVorbisCommentBlock(d []byte) *vorbisTags {
	t := &vorbisTags{}
	if len(d) < 8 {
		return t
	}
	p := 0
	vlen := int(leUint32(d[p:]))
	p += 4
	if vlen < 0 || p+vlen > len(d) {
		return t
	}
	t.Vendor = trimNull(string(d[p : p+vlen]))
	p += vlen
	if p+4 > len(d) {
		return t
	}
	count := int(leUint32(d[p:]))
	p += 4
	if count < 0 || count > 100000 {
		return t
	}
	for i := 0; i < count && p+4 <= len(d); i++ {
		l := int(leUint32(d[p:]))
		p += 4
		if l < 0 || p+l > len(d) {
			break
		}
		item := string(d[p : p+l])
		p += l
		if eq := strings.IndexByte(item, '='); eq > 0 {
			t.Items = append(t.Items, vorbisItem{Key: item[:eq], Value: item[eq+1:]})
		}
	}
	return t
}

// buildVorbisCommentBlock 构造 Vorbis Comment 数据块
func buildVorbisCommentBlock(t *vorbisTags) []byte {
	out := make([]byte, 0, 1024)
	vendor := t.Vendor
	if vendor == "" {
		vendor = "X-Box Media Manager"
	}
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(len(vendor)))
	out = append(out, b[:]...)
	out = append(out, []byte(vendor)...)
	binary.LittleEndian.PutUint32(b[:], uint32(len(t.Items)))
	out = append(out, b[:]...)
	for _, it := range t.Items {
		s := it.Key + "=" + it.Value
		binary.LittleEndian.PutUint32(b[:], uint32(len(s)))
		out = append(out, b[:]...)
		out = append(out, []byte(s)...)
	}
	return out
}

// readVorbisFile 读取 FLAC / OGG / Opus 的标签
// isFLAC=true 时按 FLAC metadata block 布局解析；否则按 OGG 页解析
func readVorbisFile(path string, isFLAC bool) (MediaMeta, error) {
	var m MediaMeta
	f, err := os.Open(path)
	if err != nil {
		return m, err
	}
	defer f.Close()

	var tag *vorbisTags
	if isFLAC {
		tag = readFLACComment(f)
	} else {
		tag = readOggComment(f)
	}
	if tag == nil {
		return m, errors.New("未找到 Vorbis Comment")
	}
	m.Title = tag.get("TITLE")
	m.Artist = tag.get("ARTIST")
	if m.Artist == "" {
		m.Artist = tag.get("PERFORMER")
	}
	m.Album = tag.get("ALBUM")
	m.Genre = tag.get("GENRE")
	m.Comment = tag.get("COMMENT")
	m.Year = parseYear(tag.get("DATE"))
	if m.Year == 0 {
		m.Year = parseYear(tag.get("YEAR"))
	}
	m.Track, m.TrackTotal = parseTrack(tag.get("TRACKNUMBER"))
	if m.TrackTotal == 0 {
		m.TrackTotal, _ = parseTrack(tag.get("TRACKTOTAL"))
	}
	if m.TrackTotal == 0 {
		m.TrackTotal, _ = parseTrack(tag.get("TOTALTRACKS"))
	}
	m.Lyrics = tag.get("LYRICS")
	if m.Lyrics == "" {
		m.Lyrics = tag.get("UNSYNCEDLYRICS")
	}
	m.HasLyric = m.Lyrics != ""
	m.HasCover = len(tag.Pics) > 0
	return m, nil
}

// readFLACComment 从 FLAC 文件读 VORBIS_COMMENT 与 PICTURE 块
func readFLACComment(f *os.File) *vorbisTags {
	head := make([]byte, 4)
	if _, err := f.ReadAt(head, 0); err != nil || string(head) != "fLaC" {
		return nil
	}
	off := int64(4)
	for {
		b := make([]byte, 4)
		if _, err := f.ReadAt(b, off); err != nil {
			return nil
		}
		last := b[0]&0x80 != 0
		blkType := b[0] & 0x7F
		length := int(beUint24(b[1:4]))
		data := make([]byte, length)
		if _, err := f.ReadAt(data, off+4); err != nil {
			return nil
		}
		if blkType == 4 { // VORBIS_COMMENT
			t := parseVorbisCommentBlock(data)
			// 继续找 PICTURE 块
			p := off + 4 + int64(length)
			for {
				pb := make([]byte, 4)
				if _, err := f.ReadAt(pb, p); err != nil {
					return t
				}
				plast := pb[0]&0x80 != 0
				ptype := pb[0] & 0x7F
				plen := int(beUint24(pb[1:4]))
				pdata := make([]byte, plen)
				if _, err := f.ReadAt(pdata, p+4); err != nil {
					return t
				}
				if ptype == 6 { // PICTURE
					if pic := parseFlacPicture(pdata); pic != nil {
						t.Pics = append(t.Pics, pic)
					}
				}
				p += 4 + int64(plen)
				if plast {
					return t
				}
			}
		}
		off += 4 + int64(length)
		if last {
			return nil
		}
	}
}

// parseFlacPicture 提取 PICTURE 块中的图片数据
func parseFlacPicture(d []byte) []byte {
	if len(d) < 32 {
		return nil
	}
	p := 4 // picture type
	// mime length
	ml := int(beUint32(d[p:]))
	p += 4
	if p+ml > len(d) {
		return nil
	}
	p += ml
	// description length
	dl := int(beUint32(d[p:]))
	p += 4
	if p+dl > len(d) {
		return nil
	}
	p += dl
	// width, height, depth, colors
	p += 16
	if p+4 > len(d) {
		return nil
	}
	dataLen := int(beUint32(d[p:]))
	p += 4
	if p+dataLen > len(d) {
		return nil
	}
	return d[p : p+dataLen]
}

// readOggComment 从 OGG/Opus 文件读评论头
func readOggComment(f *os.File) *vorbisTags {
	// 扫描所有页，收集含 Vorbis/Opus 头注释的 packet
	var packets [][]byte
	off := int64(0)
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	for off < fi.Size() && len(packets) < 4 {
		head := make([]byte, 27)
		if _, err := f.ReadAt(head, off); err != nil {
			break
		}
		if string(head[:4]) != "OggS" {
			break
		}
		nseg := int(head[26])
		segTable := make([]byte, nseg)
		if _, err := f.ReadAt(segTable, off+27); err != nil {
			break
		}
		dataLen := 0
		for _, s := range segTable {
			dataLen += int(s)
		}
		pkt := make([]byte, dataLen)
		if _, err := f.ReadAt(pkt, off+27+int64(nseg)); err != nil {
			break
		}
		packets = append(packets, pkt)
		off += 27 + int64(nseg) + int64(dataLen)
	}
	for _, pkt := range packets {
		// Vorbis 注释头："\x03vorbis" + vendor + comments
		if len(pkt) > 7 && string(pkt[:7]) == "\x03vorbis" {
			t := parseVorbisCommentBlock(pkt[7:])
			return t
		}
		// Opus 标签头："OpusTags" + vendor + comments
		if len(pkt) > 8 && string(pkt[:8]) == "OpusTags" {
			t := parseVorbisCommentBlock(pkt[8:])
			return t
		}
	}
	return nil
}

// writeVorbisTags 写回 Vorbis Comment（FLAC / OGG）
func writeVorbisTags(path string, m MediaMeta, cover []byte) error {
	if strings.EqualFold(strings.TrimPrefix(strings.ToLower(path), "."), "") {
		return errors.New("路径无效")
	}
	ext := strings.ToLower(path)
	if strings.HasSuffix(ext, ".flac") {
		return writeFLACComment(path, m, cover)
	}
	return writeOggComment(path, m, cover)
}

// writeFLACComment 重写 FLAC 的 VORBIS_COMMENT / PICTURE 块
func writeFLACComment(path string, m MediaMeta, cover []byte) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()
	fi, err := src.Stat()
	if err != nil {
		return err
	}
	// 读全部 metadata block
	head := make([]byte, 4)
	if _, err := src.ReadAt(head, 0); err != nil || string(head) != "fLaC" {
		return errors.New("不是 FLAC 文件")
	}
	off := int64(4)
	var blocks [][]byte // 含头的完整块
	var audioStart int64
	for {
		b := make([]byte, 4)
		if _, err := src.ReadAt(b, off); err != nil {
			return err
		}
		last := b[0]&0x80 != 0
		blkType := b[0] & 0x7F
		length := int(beUint24(b[1:4]))
		// 块长度必须落在文件内，否则是损坏文件或偏移算错（继续读只会读到垃圾）
		if length < 0 || off+4+int64(length) > fi.Size() {
			return errors.New("FLAC 元数据块长度越界")
		}
		data := make([]byte, length)
		if _, err := src.ReadAt(data, off+4); err != nil {
			return err
		}
		newLen := length
		// 替换 comment 块
		if blkType == 4 {
			t := parseVorbisCommentBlock(data)
			applyMetaToVorbis(t, m)
			data = buildVorbisCommentBlock(t)
			newLen = len(data)
		}
		blk := make([]byte, 0, 4+newLen)
		blk = append(blk, blkType, byte(newLen>>16), byte(newLen>>8), byte(newLen))
		blk = append(blk, data...)
		blocks = append(blocks, blk)
		// 关键：偏移必须按「原块长度」推进。用 newLen 会在 comment 变长/变短后
		// 从块中间继续读，下一个长度字段就是垃圾 → 越界或 EOF
		off += 4 + int64(length)
		if last {
			audioStart = off
			break
		}
	}
	// 封面：替换第一个 PICTURE 块
	if cover != nil {
		pic := buildFlacPicture(cover)
		replaced := false
		for i, blk := range blocks {
			if blk[0]&0x7F == 6 {
				blk = append([]byte{6, byte(len(pic) >> 16), byte(len(pic) >> 8), byte(len(pic))}, pic...)
				blocks[i] = blk
				replaced = true
				break
			}
		}
		if !replaced {
			// 插到 STREAMINFO 之后
			newBlk := append([]byte{6, byte(len(pic) >> 16), byte(len(pic) >> 8), byte(len(pic))}, pic...)
			if len(blocks) > 0 {
				blocks = append(blocks[:1], append([][]byte{newBlk}, blocks[1:]...)...)
			} else {
				blocks = append(blocks, newBlk)
			}
		}
	}
	// 重新打 last-block 标志：上面重建块头时把原 last 位清掉了，
	// 不补回去解码器会把元数据当音频帧解析，整个文件就废了
	for i := range blocks {
		blocks[i][0] &= 0x7F
	}
	if len(blocks) > 0 {
		blocks[len(blocks)-1][0] |= 0x80
	}

	// 写临时文件
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
	out.Write([]byte("fLaC"))
	for _, blk := range blocks {
		out.Write(blk)
	}
	if _, err := src.Seek(audioStart, io.SeekStart); err != nil {
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		return err
	}
	out.Sync()
	out.Close()
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

// buildFlacPicture 构造 PICTURE 块
func buildFlacPicture(img []byte) []byte {
	out := make([]byte, 0, len(img)+64)
	out = append(out, 0, 0, 0, 3) // type=3 (front cover)
	mime := "image/jpeg"
	out = append(out, byte(len(mime)>>24), byte(len(mime)>>16), byte(len(mime)>>8), byte(len(mime)))
	out = append(out, []byte(mime)...)
	out = append(out, 0, 0, 0, 0) // description length 0
	out = append(out, 0, 0, 0, 0, 0, 0, 0, 0) // width/height
	out = append(out, 0, 0, 0, 24) // depth
	out = append(out, 0, 0, 0, 0)  // colors
	out = append(out, byte(len(img)>>24), byte(len(img)>>16), byte(len(img)>>8), byte(len(img)))
	out = append(out, img...)
	return out
}

// applyMetaToVorbis 把 MediaMeta 写进 Vorbis 标签集合
func applyMetaToVorbis(t *vorbisTags, m MediaMeta) {
	t.set("TITLE", m.Title)
	t.set("ARTIST", m.Artist)
	t.set("ALBUM", m.Album)
	if m.Year > 0 {
		t.set("DATE", strconv.Itoa(m.Year))
	}
	if m.Track > 0 {
		if m.TrackTotal > 0 {
			t.set("TRACKNUMBER", strconv.Itoa(m.Track))
			t.set("TRACKTOTAL", strconv.Itoa(m.TrackTotal))
		} else {
			t.set("TRACKNUMBER", strconv.Itoa(m.Track))
		}
	}
	if m.Genre != "" {
		t.set("GENRE", m.Genre)
	}
	if m.Comment != "" {
		t.set("COMMENT", m.Comment)
	}
	if m.Lyrics != "" {
		t.set("LYRICS", m.Lyrics)
		t.set("UNSYNCEDLYRICS", m.Lyrics)
	}
}

// writeOggComment 重写 OGG 的评论头
// OGG 结构复杂（页校验和、序列号），这里采用"仅替换第一个评论头页并重排后续页"的简化策略：
// 重新序列化所有页（重算 CRC 与序列号不现实），因此改为：
//   保留原文件，仅在能安全替换时执行；否则返回错误提示不支持。
func writeOggComment(path string, m MediaMeta, cover []byte) error {
	return errors.New("OGG/Opus 标签写入暂不支持，请使用 FLAC 或 MP3")
}
