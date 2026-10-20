// 媒体标签解析：纯 Go 标准库实现，不引入第三方依赖（项目硬约束）。
//
// 覆盖：
//   音频标签读取/写入
//     - ID3v2.3 / ID3v2.4（MP3、AAC、FLAC 内嵌）：TIT2/TPE1/TALB/TRCK/TYER/TDRC/TPE2/TPOS/TCON
//     - Vorbis Comment（FLAC、OGG、Opus）：TITLE/ARTIST/ALBUM/TRACKNUMBER/DATE
//     - MP4 ilst（M4A、M4B、AAC in MP4）：©nam/©ART/©alb/©day/trkn/covr
//   视频元数据读取
//     - MP4/MOV：moov → mvhd(时长/创建时间)、trak→tkhd(宽高)、udta→meta→ilst
//     - Matroska/WebM（EBML）：Info→Title、Duration
//
// 设计要点：
//   - 只解析需要的字段，避免整文件读入（大视频动辄数 GB）。
//   - 读取用 Seek + 精确长度的 Read；写入采用"临时文件 + 原子改名"，
//     中途失败不会破坏原文件。
package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// MediaMeta 统一的媒体元数据（视频/音频共用）
type MediaMeta struct {
	Title    string // 标题（片名 / 歌名）
	Artist   string // 歌手 / 导演
	Album    string // 专辑
	Year     int    // 年份
	Track    int    // 音轨号
	TrackTotal int  // 总音轨数
	Genre    string
	Duration float64 // 秒
	Width    int     // 视频宽
	Height   int     // 视频高
	Comment  string
	HasCover bool
	HasLyric bool
	Lyrics   string
	Format   string // 容器格式：mp4 / flac / mp3 / matroska ...
}

// 视频分辨率标签
func (m MediaMeta) Resolution() string {
	switch {
	case m.Width <= 0 || m.Height <= 0:
		return ""
	case m.Height >= 2000 || m.Width >= 3800:
		return "4K"
	case m.Height >= 1400 || m.Width >= 2500:
		return "2160p"
	case m.Height >= 1000 || m.Width >= 1800:
		return "1080p"
	case m.Height >= 700 || m.Width >= 1200:
		return "720p"
	case m.Height >= 460 || m.Width >= 800:
		return "480p"
	default:
		return fmt.Sprintf("%dp", m.Height)
	}
}

// ---------- 通用工具 ----------

// readAt 从 f 的 off 处读 n 字节（不足则返回实际字节数与错误）
func readAt(f *os.File, off int64, n int) ([]byte, error) {
	buf := make([]byte, n)
	got, err := f.ReadAt(buf, off)
	if err != nil && got < n {
		return buf[:got], err
	}
	return buf[:got], nil
}

// beUint32 / beUint24 读大端整数
func beUint32(b []byte) uint32 {
	if len(b) < 4 {
		return 0
	}
	return binary.BigEndian.Uint32(b)
}

func beUint24(b []byte) uint32 {
	if len(b) < 3 {
		return 0
	}
	return uint32(b[0])<<16 | uint32(b[1])<<8 | uint32(b[2])
}

func beUint16(b []byte) uint16 {
	if len(b) < 2 {
		return 0
	}
	return binary.BigEndian.Uint16(b)
}

// leUint32 读小端整数（EBML 用）
func leUint32(b []byte) uint32 {
	if len(b) < 4 {
		return 0
	}
	return binary.LittleEndian.Uint32(b)
}

// decodeText 解码文本标签（尽量识别 UTF-16/UTF-8/Latin1）
func decodeText(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	// UTF-16 BOM（无编码字节前缀的情况）
	// FF FE = 小端，FE FF = 大端；decodeUTF16LE 的 swap=true 表示按大端读
	if len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE {
		return trimNull(decodeUTF16LE(b[2:], false))
	}
	if len(b) >= 2 && b[0] == 0xFE && b[1] == 0xFF {
		return trimNull(decodeUTF16LE(b[2:], true))
	}
	// ID3 文本帧自带编码字节
	if len(b) >= 1 && (b[0] == 0x00 || b[0] == 0x01 || b[0] == 0x02 || b[0] == 0x03) {
		enc := b[0]
		body := b[1:]
		switch enc {
		case 0x00:
			return trimNull(string(body))
		case 0x01:
			// UTF-16 带 BOM：按 BOM 判定字节序；无 BOM 时按规范默认小端
			if len(body) >= 2 && body[0] == 0xFF && body[1] == 0xFE {
				return trimNull(decodeUTF16LE(body[2:], false))
			}
			if len(body) >= 2 && body[0] == 0xFE && body[1] == 0xFF {
				return trimNull(decodeUTF16LE(body[2:], true))
			}
			return trimNull(decodeUTF16LE(body, false))
		case 0x02:
			// UTF-16BE 无 BOM
			return trimNull(beUTF16BE(body))
		case 0x03:
			return trimNull(string(body))
		}
	}
	// 默认 UTF-8，非法字节则退回 Latin-1
	s := trimNull(string(b))
	if !isUTF8(s) {
		return latin1(b)
	}
	return s
}

func decodeUTF16LE(b []byte, swap bool) string {
	var sb strings.Builder
	for i := 0; i+1 < len(b); i += 2 {
		u := uint16(b[i]) | uint16(b[i+1])<<8
		if swap {
			u = uint16(b[i])<<8 | uint16(b[i+1])
		}
		sb.WriteRune(rune(u))
	}
	return trimNull(sb.String())
}

func beUTF16BE(b []byte) string {
	var sb strings.Builder
	for i := 0; i+1 < len(b); i += 2 {
		u := uint16(b[i])<<8 | uint16(b[i+1])
		sb.WriteRune(rune(u))
	}
	return sb.String()
}

func latin1(b []byte) string {
	var sb strings.Builder
	for _, c := range b {
		sb.WriteRune(rune(c))
	}
	return trimNull(sb.String())
}

func trimNull(s string) string {
	s = strings.TrimRight(s, "\x00")
	return strings.TrimSpace(s)
}

func isUTF8(s string) bool {
	for _, r := range s {
		if r == 0xFFFD {
			return false
		}
	}
	return true
}

// ---------- 入口：按扩展名分派 ----------

// sniffFormat 读文件头判断容器格式（比只看扩展名可靠）
func sniffFormat(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := make([]byte, 16)
	n, _ := io.ReadFull(f, h) // 必须读满：短读会让 GUID / ftyp 判断失效
	h = h[:n]

	// ID3 → 后面是 MP3
	if len(h) >= 3 && h[0] == 'I' && h[1] == 'D' && h[2] == '3' {
		return "mp3"
	}
	// MP4/MOV：ftyp box
	if len(h) >= 8 && string(h[4:8]) == "ftyp" {
		return "mp4"
	}
	// Matroska/WebM：EBML 头
	if len(h) >= 4 && h[0] == 0x1A && h[1] == 0x45 && h[2] == 0xDF && h[3] == 0xA3 {
		return "matroska"
	}
	// FLAC
	if len(h) >= 4 && string(h[:4]) == "fLaC" {
		return "flac"
	}
	// ASF（WMA / WMV）
	if isASF(h) {
		return "asf"
	}
	// Ogg
	if len(h) >= 4 && string(h[:4]) == "OggS" {
		return "ogg"
	}
	// RIFF（wav/avif）
	if len(h) >= 4 && string(h[:4]) == "RIFF" {
		return "riff"
	}
	return ""
}

// mediaExts 视为视频的扩展名
var mediaVideoExts = map[string]bool{
	".mp4": true, ".mkv": true, ".mov": true, ".avi": true, ".wmv": true,
	".flv": true, ".m4v": true, ".ts": true, ".m2ts": true, ".webm": true,
	".mpg": true, ".mpeg": true, ".rmvb": true, ".rm": true, ".vob": true,
	".iso": true, ".3gp": true, ".mxf": true,
}

// mediaAudioExts 视为音频的扩展名
var mediaAudioExts = map[string]bool{
	".mp3": true, ".flac": true, ".wav": true, ".aac": true, ".m4a": true,
	".m4b": true, ".ogg": true, ".oga": true, ".opus": true, ".wma": true,
	".ape": true, ".alac": true, ".aiff": true, ".aif": true, ".dsf": true,
	".mid": true, ".midi": true,
}

// IsVideoExt / IsAudioExt 扩展名分类
func IsVideoExt(path string) bool { return mediaVideoExts[strings.ToLower(filepath.Ext(path))] }
func IsAudioExt(path string) bool { return mediaAudioExts[strings.ToLower(filepath.Ext(path))] }

// ReadMediaMeta 读取媒体元数据（自动识别格式）
func ReadMediaMeta(path string) (MediaMeta, error) {
	var m MediaMeta
	format := sniffFormat(path)
	ext := strings.ToLower(filepath.Ext(path))
	switch {
	case format == "matroska":
		m, _ = readMatroska(path)
		m.Format = "matroska"
	case format == "mp4":
		m, _ = readMP4(path)
		m.Format = "mp4"
	case format == "asf":
		m, _ = readASF(path)
		m.Format = "asf"
	case format == "flac":
		m, _ = readVorbisFile(path, true)
		m.Format = "flac"
	case format == "ogg":
		// Ogg 容器内可能是 Vorbis 或 Opus
		if isOpus(path) {
			m, _ = readVorbisFile(path, false)
			m.Format = "opus"
		} else {
			m, _ = readVorbisFile(path, false)
			m.Format = "ogg"
		}
	case format == "mp3" || ext == ".mp3":
		m, _ = readID3(path)
		m.Format = "mp3"
	case ext == ".m4a" || ext == ".m4b" || ext == ".aac":
		// 无 ftyp 但扩展名对：仍尝试按 MP4 解析
		if mm, err := readMP4(path); err == nil {
			mm.Format = "mp4"
			return mm, nil
		}
		return m, errors.New("无法解析 MP4 元数据")
	default:
		// 回退：按扩展名尝试
		switch ext {
		case ".wav":
			m, _ = readWAV(path)
			m.Format = "wav"
		case ".mp4", ".m4v", ".mov":
			m, _ = readMP4(path)
			m.Format = "mp4"
		case ".webm", ".mkv":
			m, _ = readMatroska(path)
			m.Format = "matroska"
		case ".wma", ".wmv", ".asf":
			m, _ = readASF(path)
			m.Format = "asf"
		}
	}
	return m, nil
}

// isOpus 判断 Ogg 内容是 Opus（页首 "OpusHead"）
func isOpus(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 64)
	n, _ := f.Read(buf)
	return strings.Contains(string(buf[:n]), "OpusHead")
}
