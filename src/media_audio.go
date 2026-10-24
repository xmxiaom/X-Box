package main

// 音频整理：读取标签 → 字段纠错 → 在线补全 → 规范命名/归档 → 标签回写。
//
// 命名规则（默认）：歌手 - 歌名.ext
// 归档模式（organize=true）：歌手/专辑/01 - 歌名.ext
// 支持自定义模板：{artist} {title} {album} {year} {track}

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// audioNamePlan 一次音频重命名/改写计划
type audioNamePlan struct {
	OldPath    string     `json:"oldPath"`
	NewPath    string     `json:"newPath"`
	OldName    string     `json:"oldName"`
	NewName    string     `json:"newName"`
	Title      string     `json:"title"`
	Artist     string     `json:"artist"`
	Album      string     `json:"album"`
	Year       int        `json:"year"`
	Track      int        `json:"track"`
	Reason     string     `json:"reason"`
	Changed    bool       `json:"changed"`
	Fixes      []mediaFix `json:"fixes,omitempty"`
	Confidence float64    `json:"confidence,omitempty"`
	Warn       string     `json:"warn,omitempty"`
	// RoleGuessed 表示歌手/歌名在文件名里的位置是"猜"的（目录里没有重复证据），
	// 这种文件只改名、不回写标签，避免把推断错的角色写进文件
	RoleGuessed bool `json:"roleGuessed,omitempty"`
}

// renderAudioName 用模板渲染文件名（不含目录部分）
func renderAudioName(tmpl string, m MediaMeta) string {
	if strings.TrimSpace(tmpl) == "" {
		tmpl = "{title} - {artist}"
	}
	year := ""
	if m.Year > 0 {
		year = strconv.Itoa(m.Year)
	}
	track := ""
	if m.Track > 0 {
		track = fmt.Sprintf("%02d", m.Track)
	}
	r := strings.NewReplacer(
		"{title}", m.Title,
		"{artist}", m.Artist,
		"{album}", m.Album,
		"{year}", year,
		"{track}", track,
	)
	name := sanitizeName(r.Replace(tmpl))
	// 模板占位全为空时会渲染出 " - " 之类，收敛为空
	name = strings.Trim(strings.TrimSpace(name), "-_·|")
	name = strings.TrimSpace(name)
	return name
}

// audioDirFor 计算归档目录：歌手/专辑；缺项时逐级退让
func audioDirFor(root string, m MediaMeta) string {
	artist := sanitizeName(m.Artist)
	album := sanitizeName(m.Album)
	if artist == "" {
		artist = "未知歌手"
	}
	dir := filepath.Join(root, artist)
	if album != "" {
		dir = filepath.Join(dir, album)
	}
	return dir
}

// planAudioRename 为单个音频文件生成整理计划。
// opts 控制是否纠错/联网/归档；reserved 用于同批次内避免重名冲突。
// 返回值：计划、纠错后的元数据（供标签回写）、在线匹配到的封面（可能为 nil）。
func planAudioRename(path string, opts mediaOptions, reserved map[string]bool) (audioNamePlan, MediaMeta, []byte) {
	plan := audioNamePlan{OldPath: path, NewPath: path}
	plan.OldName = filepath.Base(path)
	plan.NewName = plan.OldName
	var cover []byte

	meta, _ := ReadMediaMeta(path)
	// 文件名本身也可能是乱码（旧工具按西文页解过一遍），先还原再解析
	nameNoExt := fixMojibake(strings.TrimSuffix(plan.OldName, filepath.Ext(plan.OldName)))

	// 1) 字段纠错：文件名 + 现有标签交叉印证
	corrected := correctedMeta{Meta: meta, Confidence: 0.5}
	if opts.Fix {
		corrected = correctAudioTags(meta, nameNoExt, opts.ArtistSecond, path)
	}
	// 标签为空 + 角色靠猜：只改名不回写标签
	if opts.RoleGuessed && strings.TrimSpace(meta.Title) == "" && strings.TrimSpace(meta.Artist) == "" {
		plan.RoleGuessed = true
	}

	// 2) 在线补全（MusicBrainz → iTunes）
	if opts.Online {
		if online := matchMusicOnline(corrected.Meta.Title, corrected.Meta.Artist, corrected.Meta.Album); online.OK {
			corrected = mergeCorrected(corrected, online.Meta, true)
			if online.Lyrics != "" && corrected.Meta.Lyrics == "" {
				corrected.Meta.Lyrics = online.Lyrics
			}
			cover = online.Cover
		} else if online.Err != "" {
			plan.Warn = online.Err
		}
	}

	m := corrected.Meta
	// 兜底：标签全空时用文件名当歌名，避免整批变成"未命名"
	if strings.TrimSpace(m.Title) == "" {
		m.Title = cleanTitle(nameNoExt)
	}
	plan.Title, plan.Artist, plan.Album = m.Title, m.Artist, m.Album
	plan.Year, plan.Track = m.Year, m.Track
	plan.Fixes = corrected.Fixes
	plan.Confidence = corrected.Confidence

	// 3) 生成文件名
	ext := filepath.Ext(path)
	newName := renderAudioName(opts.Template, m)
	if newName == "" {
		newName = sanitizeName(m.Title)
	}
	if newName == "" {
		plan.Reason = "信息不足，保持原名"
		return plan, m, cover
	}
	newName += ext

	newDir := filepath.Dir(path)
	switch {
	case opts.Flatten:
		// 去除子目录：所有音乐平铺到任务根目录（与 Organize 互斥，UI 已保证）。
		// 基准同样必须是任务根目录，不能用文件当前目录——否则深层的文件
		// 只会被提到它的上一级，永远平不干净
		base := strings.TrimSpace(opts.Root)
		if base == "" {
			base = filepath.Dir(path)
		}
		newDir = base
	case opts.Organize:
		// 基准必须是任务根目录：用当前所在目录会在每次整理时把
		// 「歌手/专辑」再套一层，越整理越深
		base := strings.TrimSpace(opts.Root)
		if base == "" {
			base = filepath.Dir(path)
		}
		newDir = audioDirFor(base, m)
	}
	target := uniquePath(path, filepath.Join(newDir, newName), reserved)

	plan.NewPath = target
	plan.NewName = filepath.Base(target)
	plan.Changed = target != path
	if plan.Changed {
		switch {
		case opts.Flatten:
			plan.Reason = "去除子目录，平铺到任务目录"
		case plan.NewName != plan.OldName:
			plan.Reason = "按规范重命名"
		default:
			plan.Reason = "归档到歌手/专辑目录"
		}
	}
	return plan, m, cover
}

// writeMediaTags 按扩展名分派标签写入（临时文件 + 原子改名，失败不破坏原文件）
func writeMediaTags(path string, m MediaMeta, cover []byte) error {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp3", ".aac":
		return writeID3Tags(path, m, cover)
	case ".flac":
		return writeFLACComment(path, m, cover)
	case ".ogg", ".oga", ".opus":
		return writeOggComment(path, m, cover)
	case ".m4a", ".m4b", ".mp4", ".m4v":
		return writeMP4Tags(path, m, cover)
	case ".wma", ".asf", ".wmv":
		return writeASFTags(path, m, cover)
	}
	return errTagUnsupported
}

// errTagUnsupported 格式本身不支持写标签（不是错误，不该标红）
var errTagUnsupported = errors.New("该格式不支持写入标签")

// moveMediaFile 移动文件（同设备改名，跨设备退回复制+删除）
func moveMediaFile(oldPath, newPath string) error {
	if oldPath == newPath {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
		return err
	}
	if err := os.Rename(oldPath, newPath); err == nil {
		return nil
	}
	// 跨设备改名返回 EXDEV：退回复制后删除源文件
	if err := copyFileLocal(oldPath, newPath); err != nil {
		return err
	}
	return os.Remove(oldPath)
}

// copyFileLocal 原样复制文件（保留权限位），用于跨设备移动
func copyFileLocal(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}
