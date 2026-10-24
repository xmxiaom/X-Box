// ID3v2 标签读写（MP3 / AAC / FLAC 内嵌）
//
// 支持版本：ID3v2.3、ID3v2.4（v2.2 帧 ID 为 3 字节，也兼容）。
// 帧 ID：TIT2(标题) TPE1(歌手) TALB(专辑) TRCK(音轨) TYER/TDRC(年份)
//        TPE2(专辑艺术家) TPOS(碟片) TCON(流派) COMM(备注) USLT(歌词) APIC(封面)
package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// id3Frame 一帧标签
type id3Frame struct {
	ID    string
	Value []byte
	Flags uint16
}

// id3Tag 解析结果
type id3Tag struct {
	Major  int
	Frames []id3Frame
	// 原始 ID3 头之后的数据区（用于写回时保留未知帧）
	Size   int
	Header []byte
}

// readID3Tag 读取文件的 ID3v2 标签（返回 nil 表示无标签）
// id3SynchSafe 解码 synchsafe 整数（每字节低 7 位）
func id3SynchSafe(b []byte) int {
	if len(b) < 4 {
		return 0
	}
	return int(b[0]&0x7f)<<21 | int(b[1]&0x7f)<<14 | int(b[2]&0x7f)<<7 | int(b[3]&0x7f)
}

func readID3Tag(f *os.File) (*id3Tag, error) {
	head := make([]byte, 10)
	if _, err := io.ReadFull(f, head); err != nil {
		return nil, err
	}
	if head[0] != 'I' || head[1] != 'D' || head[2] != '3' {
		return nil, errors.New("无 ID3 标签")
	}
	major := int(head[3])
	flags := head[5]
	// 长度是 synchsafe 整数：每字节只用低 7 位，不能当普通大端整数读
	// （普通读法在标签 >127 字节时算出偏大的值，导致 io.ReadFull 半路 EOF）
	size := id3SynchSafe(head[6:10])
	tag := &id3Tag{Major: major, Size: size, Header: append([]byte{}, head...)}

	body := make([]byte, size)
	if _, err := io.ReadFull(f, body); err != nil {
		return nil, err
	}
	// 扩展头
	if flags&0x40 != 0 && len(body) >= 4 {
		ext := int(beUint32(body[0:4]))
		if major >= 4 {
			ext = id3SynchSafe(body[0:4])
		}
		if ext > 0 && ext < len(body) {
			body = body[ext:]
		}
	}
	tag.Frames = parseID3Frames(body, major)
	return tag, nil
}

// parseID3Frames 解析帧序列
func parseID3Frames(body []byte, major int) []id3Frame {
	var out []id3Frame
	p := 0
	for p+10 <= len(body) {
		id := string(body[p : p+4])
		if id == "\x00\x00\x00\x00" {
			break // padding
		}
		if !isID3FrameID(id) {
			break
		}
		var fsize int
		var fflags uint16
		if major >= 4 {
			fsize = int(beUint32(body[p+4 : p+8]) & 0x0FFFFFFF)
			fflags = beUint16(body[p+8 : p+10])
		} else {
			fsize = int(beUint32(body[p+4 : p+8]))
			fflags = beUint16(body[p+8 : p+10])
		}
		p += 10
		if fsize < 0 || p+fsize > len(body) {
			break
		}
		out = append(out, id3Frame{ID: id, Value: body[p : p+fsize], Flags: fflags})
		p += fsize
	}
	return out
}

// isID3FrameID 校验帧 ID 是 4 个大写字母或数字
func isID3FrameID(id string) bool {
	if len(id) != 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		c := id[i]
		if !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

// id3FrameValue 取帧的文本值（自动跳过编码字节与可能的附加信息）
func (t *id3Tag) id3FrameValue(id string) string {
	for _, f := range t.Frames {
		if f.ID == id {
			return decodeText(f.Value)
		}
	}
	return ""
}

// id3FrameValueAny 取多候选帧的第一个非空值
func (t *id3Tag) id3FrameValueAny(ids ...string) string {
	for _, id := range ids {
		if v := t.id3FrameValue(id); v != "" {
			return v
		}
	}
	return ""
}

// readID3 读取 MP3 的 ID3 元数据
func readID3(path string) (MediaMeta, error) {
	var m MediaMeta
	f, err := os.Open(path)
	if err != nil {
		return m, err
	}
	defer f.Close()
	tag, err := readID3Tag(f)
	if err != nil {
		return m, err
	}
	m.Title = tag.id3FrameValue("TIT2")
	m.Artist = tag.id3FrameValueAny("TPE1", "TPE3")
	m.Album = tag.id3FrameValue("TALB")
	m.Genre = parseID3Genre(tag.id3FrameValueAny("TCON", "TFLT"))
	m.Comment = tag.id3FrameValue("COMM")
	m.Year = parseYear(tag.id3FrameValueAny("TDRC", "TYER", "TDAT"))
	m.Track, m.TrackTotal = parseTrack(tag.id3FrameValue("TRCK"))
	// 歌词（USLT 带描述头：编码1字节 + 语言3字节 + 描述(终止符)）
	for _, f := range tag.Frames {
		if f.ID == "USLT" && len(f.Value) > 4 {
			body := f.Value[1:]
			if len(body) > 3 {
				body = body[3:]
			}
			// 跳过描述（以 0 结尾）
			if i := bytes.IndexByte(body, 0); i >= 0 {
				body = body[i+1:]
			}
			m.Lyrics = decodeText(body)
			m.HasLyric = m.Lyrics != ""
			break
		}
	}
	for _, f := range tag.Frames {
		if f.ID == "APIC" {
			m.HasCover = true
			break
		}
	}
	return m, nil
}

// parseYear 从年份字符串取四位数字
func parseYear(s string) int {
	s = strings.TrimSpace(s)
	if len(s) >= 4 {
		if y, err := strconv.Atoi(s[:4]); err == nil && y > 1800 && y < 2200 {
			return y
		}
	}
	// 找第一个 4 位数字
	for i := 0; i+4 <= len(s); i++ {
		if y, err := strconv.Atoi(s[i : i+4]); err == nil && y > 1800 && y < 2200 {
			return y
		}
	}
	return 0
}

// parseTrack 解析 "3/12" → (3, 12)
func parseTrack(s string) (int, int) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0
	}
	parts := strings.SplitN(s, "/", 2)
	n1, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
	n2 := 0
	if len(parts) > 1 {
		n2, _ = strconv.Atoi(strings.TrimSpace(parts[1]))
	}
	return n1, n2
}

// id3Genres ID3v1 风格流派表（用于数字引用）
var id3Genres = []string{
	"Blues", "Classic Rock", "Country", "Dance", "Disco", "Funk", "Grunge",
	"Hip-Hop", "Jazz", "Metal", "New Age", "Oldies", "Other", "Pop", "R&B",
	"Rap", "Reggae", "Rock", "Techno", "Industrial", "Alternative", "Ska",
	"Death Metal", "Pranks", "Soundtrack", "Euro-Techno", "Ambient", "Trip-Hop",
	"Vocal", "Jazz+Funk", "Fusion", "Trance", "Classical", "Instrumental",
	"Acid", "House", "Game", "Sound Clip", "Gospel", "Noise", "Alt. Rock",
	"Bass", "Soul", "Punk", "Space", "Meditative", "Instrumental Pop",
	"Instrumental Rock", "Ethnic", "Gothic", "Darkwave", "Techno-Industrial",
	"Electronic", "Pop-Folk", "Eurodance", "Dream", "Southern Rock", "Comedy",
	"Cult", "Gangsta Rap", "Top 40", "Christian Rap", "Pop/Funk", "Jungle",
	"Native American", "Cabaret", "New Wave", "Psychedelic", "Rave", "Showtunes",
	"Trailer", "Lo-Fi", "Tribal", "Acid Punk", "Acid Jazz", "Polka", "Retro",
	"Musical", "Rock & Roll", "Hard Rock", "Folk", "Folk/Rock", "National Folk",
	"Swing", "Fast-Fusion", "Bebop", "Latin", "Revival", "Celtic", "Bluegrass",
	"Avantgarde", "Gothic Rock", "Progressive Rock", "Psychedelic Rock",
	"Symphonic Rock", "Slow Rock", "Big Band", "Chorus", "Easy Listening", "Acoustic",
	"Humour", "Speech", "Chanson", "Opera", "Chamber Music", "Sonata", "Symphony",
	"Booty Bass", "Primus", "Porn Groove", "Satire", "Slow Jam", "Club", "Tango",
	"Samba", "Folklore", "Ballad", "Power Ballad", "Rhythmic Soul", "Freestyle",
	"Duet", "Punk Rock", "Drum Solo", "A Cappella", "Euro-House", "Dance Hall",
}

// parseID3Genre 处理数字引用流派
func parseID3Genre(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if n, err := strconv.Atoi(s); err == nil && n >= 0 && n < len(id3Genres) {
		return id3Genres[n]
	}
	// "(17)" 或 "17" 形式
	s = strings.Trim(s, "()")
	if n, err := strconv.Atoi(s); err == nil && n >= 0 && n < len(id3Genres) {
		return id3Genres[n]
	}
	return s
}

// ---------- 写 ID3 ----------

// id3BuildFrame 构造一个 ID3v2.3 文本帧
func id3BuildFrame(id, text string) []byte {
	if text == "" {
		return nil
	}
	// 用 UTF-16 带 BOM（编码 1），中文安全
	utf16 := make([]byte, 0, len(text)*2+4)
	utf16 = append(utf16, 0xFF, 0xFE)
	for _, r := range text {
		if r > 0xFFFF {
			r = 0xFFFD
		}
		var buf [2]byte
		binary.LittleEndian.PutUint16(buf[:], uint16(r))
		utf16 = append(utf16, buf[0], buf[1])
	}
	payload := append([]byte{0x01}, utf16...)
	out := make([]byte, 0, 10+len(payload))
	out = append(out, []byte(id)...)
	var sz [4]byte
	binary.BigEndian.PutUint32(sz[:], uint32(len(payload)))
	out = append(out, sz[:]...)
	out = append(out, 0x00, 0x00) // flags
	out = append(out, payload...)
	return out
}

// id3BuildLyric 构造 USLT 歌词帧
func id3BuildLyric(text string) []byte {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	utf16 := make([]byte, 0, len(text)*2+4)
	utf16 = append(utf16, 0xFF, 0xFE)
	for _, r := range text {
		if r > 0xFFFF {
			r = 0xFFFD
		}
		var buf [2]byte
		binary.LittleEndian.PutUint16(buf[:], uint16(r))
		utf16 = append(utf16, buf[0], buf[1])
	}
	// 编码(1) + 语言(3) + 描述(空终止) + 文本
	payload := []byte{0x01, 'z', 'h', 'a', 0x00}
	payload = append(payload, utf16...)
	out := make([]byte, 0, 10+len(payload))
	out = append(out, []byte("USLT")...)
	var sz [4]byte
	binary.BigEndian.PutUint32(sz[:], uint32(len(payload)))
	out = append(out, sz[:]...)
	out = append(out, 0x00, 0x00)
	out = append(out, payload...)
	return out
}

// id3BuildAPIC 构造 APIC 封面帧（jpeg）
func id3BuildAPIC(cover []byte) []byte {
	if len(cover) == 0 {
		return nil
	}
	// 编码(1) + MIME(以0结尾) + 图片类型(1) + 描述(以0结尾) + 图片数据
	payload := []byte{0x00}
	payload = append(payload, []byte("image/jpeg")...)
	payload = append(payload, 0x00)
	payload = append(payload, 0x03) // 封面(正面)
	payload = append(payload, 0x00) // 空描述
	payload = append(payload, cover...)
	out := make([]byte, 0, 10+len(payload))
	out = append(out, []byte("APIC")...)
	var sz [4]byte
	binary.BigEndian.PutUint32(sz[:], uint32(len(payload)))
	out = append(out, sz[:]...)
	out = append(out, 0x00, 0x00)
	out = append(out, payload...)
	return out
}

// writeID3Tags 把标签写回 MP3 文件（原子替换）
// cover 为 nil 时不写封面；lyrics 为空时不写歌词帧。
func writeID3Tags(path string, m MediaMeta, cover []byte) error {
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()

	// 读旧标签（用于保留未涉及的帧，如封面）
	var frames []byte
	old, _ := readID3Tag(src)
	if old != nil {
		for _, f := range old.Frames {
			switch f.ID {
			case "TIT2", "TPE1", "TPE2", "TALB", "TRCK", "TYER", "TDRC", "TCON", "COMM", "USLT":
				continue // 会被覆盖
			case "APIC":
				if cover == nil {
					frames = append(frames, id3FrameBytes(f)...)
				}
				continue
			}
			frames = append(frames, id3FrameBytes(f)...)
		}
	}
	// 读音频数据起点
	audioStart := int64(0)
	if old != nil {
		audioStart = int64(10 + old.Size)
	}

	// 构造新帧
	add := func(id, val string) {
		if b := id3BuildFrame(id, val); b != nil {
			frames = append(frames, b...)
		}
	}
	add("TIT2", m.Title)
	add("TPE1", m.Artist)
	add("TALB", m.Album)
	if m.Track > 0 {
		if m.TrackTotal > 0 {
			add("TRCK", fmt.Sprintf("%d/%d", m.Track, m.TrackTotal))
		} else {
			add("TRCK", strconv.Itoa(m.Track))
		}
	}
	if m.Year > 0 {
		add("TDRC", strconv.Itoa(m.Year))
		add("TYER", strconv.Itoa(m.Year))
	}
	if m.Genre != "" {
		add("TCON", m.Genre)
	}
	if m.Comment != "" {
		add("COMM", m.Comment)
	}
	if m.Lyrics != "" {
		frames = append(frames, id3BuildLyric(m.Lyrics)...)
	}
	if cover != nil {
		frames = append(frames, id3BuildAPIC(cover)...)
	}

	// 组装 ID3v2.3 头
	head := make([]byte, 10)
	copy(head, "ID3")
	head[3] = 3
	head[4] = 0
	head[5] = 0
	head[6] = 0 // 长度 synchsafe，逐字节写
	sz := len(frames)
	head[6] = byte((sz >> 21) & 0x7F)
	head[7] = byte((sz >> 14) & 0x7F)
	head[8] = byte((sz >> 7) & 0x7F)
	head[9] = byte(sz & 0x7F)

	// 写临时文件：头 + 帧 + 原音频数据
	tmp := path + ".xboxmedia.tmp"
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
	if _, err := out.Write(head); err != nil {
		return err
	}
	if _, err := out.Write(frames); err != nil {
		return err
	}
	if _, err := src.Seek(audioStart, io.SeekStart); err != nil {
		return err
	}
	if _, err := io.Copy(out, src); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	// 保留原 mtime
	if fi, err := os.Stat(path); err == nil {
		os.Chtimes(tmp, fi.ModTime(), fi.ModTime())
	}
	// 先关源文件再改名：POSIX 允许覆盖打开中的文件，Windows 会直接
	// "Access is denied"（defer 的 Close 要到函数返回才执行，赶不上）
	src.Close()
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	okAll = true
	return nil
}

// id3FrameBytes 还原一个帧的原始字节
func id3FrameBytes(f id3Frame) []byte {
	out := make([]byte, 0, 10+len(f.Value))
	out = append(out, []byte(f.ID)...)
	var sz [4]byte
	binary.BigEndian.PutUint32(sz[:], uint32(len(f.Value)))
	out = append(out, sz[:]...)
	out = append(out, byte(f.Flags>>8), byte(f.Flags))
	out = append(out, f.Value...)
	return out
}

// id3StripTags 去掉 ID3 标签（用于提取纯音频数据）
func id3StripTags(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tag, err := readID3Tag(f)
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.NewSectionReader(f, int64(10+tag.Size), 1<<62))
	if err != nil {
		return nil, err
	}
	return data, nil
}

// mediaTempPath 生成同目录临时文件名（同分区保证 rename 原子性）
func mediaTempPath(path string) string {
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	return filepath.Join(dir, "."+base+".xboxmedia.tmp")
}
