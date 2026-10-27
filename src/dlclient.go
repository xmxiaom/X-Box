package main

// 下载板块 · 目录配置（v1.3.39 起：磁力由内置 BT 引擎完成，不再外接下载器）。
//
// 保留职责：
//   1. download.json 配置读写（视频目录 / 音乐目录）；
//   2. 缺省下载目录建议（dlDefaultDir）；
//   3. 提交历史兼容（任务即列表，历史意义弱化，不再暴露）。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DlConfig 下载板块整体配置（download.json）。v1.3.39 起不含下载器连接信息，
// 旧配置里的 clients 字段会被 json.Unmarshal 忽略。
type DlConfig struct {
	VideoDir string `json:"videoDir"` // 视频下载目录（磁力任务落盘根目录）
	MusicDir string `json:"musicDir"` // 音乐下载目录（mp3 直下 / 磁力音频）

	// 并发与线程（v1.3.43）：0 表示用引擎默认值
	MaxTasks int `json:"maxTasks,omitempty"` // 同时下载的任务数（1-8，默认 3）
	MaxConns int `json:"maxConns,omitempty"` // 单任务的并发连接数（4-40，默认 16）
	Pipeline int `json:"pipeline,omitempty"` // 单连接的挂起块数（4-32，默认 16）
}

// dlConfigPath 配置文件路径
func dlConfigPath() string {
	dir := os.Getenv("XBOX_DATA")
	if dir == "" {
		dir = "."
	}
	return filepath.Join(dir, "download.json")
}

// loadDlConfig 读配置（缺失/损坏返回零值配置）
func loadDlConfig() DlConfig {
	var cfg DlConfig
	b, err := os.ReadFile(dlConfigPath())
	if err != nil || json.Unmarshal(b, &cfg) != nil {
		return DlConfig{}
	}
	cfg.VideoDir = strings.TrimSpace(cfg.VideoDir)
	cfg.MusicDir = strings.TrimSpace(cfg.MusicDir)
	cfg.MaxTasks = dlClampInt(cfg.MaxTasks, 1, btMaxTasks, dlDefMaxTasks)
	cfg.MaxConns = dlClampInt(cfg.MaxConns, 4, 40, dlDefMaxConns)
	cfg.Pipeline = dlClampInt(cfg.Pipeline, 4, 32, dlDefPipeline)
	return cfg
}

// dlClampInt 取值夹紧（0 或越界回落默认值）
func dlClampInt(v, lo, hi, def int) int {
	if v <= 0 {
		return def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// saveDlConfig 落盘（0600）
func saveDlConfig(cfg DlConfig) error {
	cfg.VideoDir = strings.TrimSpace(cfg.VideoDir)
	cfg.MusicDir = strings.TrimSpace(cfg.MusicDir)
	cfg.MaxTasks = dlClampInt(cfg.MaxTasks, 1, btMaxTasks, dlDefMaxTasks)
	cfg.MaxConns = dlClampInt(cfg.MaxConns, 4, 40, dlDefMaxConns)
	cfg.Pipeline = dlClampInt(cfg.Pipeline, 4, 32, dlDefPipeline)
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(dlConfigPath(), b, 0o600)
}

// 并发默认值（设置页可调；调小省带宽，调大提速）
const (
	dlDefMaxTasks = 3  // 同时下载任务数
	dlDefMaxConns = 16 // 单任务并发连接
	dlDefPipeline = 16 // 单连接挂起块数（16 块 = 256KB 管线）
)

// dlDefaultDir 缺省下载目录：首个媒体根（/vol1 等）下的 downloads 子目录。
// 不主动创建（用户保存配置时才建），返回建议值。
func dlDefaultDir(kind string) string {
	roots := mediaRoots()
	for _, root := range roots {
		if root == "/" {
			continue
		}
		cand := filepath.Join(root, "downloads")
		if kind == "music" {
			cand = filepath.Join(root, "downloads", "music")
		}
		if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
			return cand
		}
	}
	if len(roots) > 0 && roots[0] != "/" {
		if kind == "music" {
			return filepath.Join(roots[0], "downloads", "music")
		}
		return filepath.Join(roots[0], "downloads")
	}
	return ""
}

// —— 兼容占位：老的提交历史结构（dlHist）已由统一任务列表取代 ——
type dlHistEntry struct {
	At     time.Time `json:"at"`
	Title  string    `json:"title"`
	Client string    `json:"client"`
	OK     bool      `json:"ok"`
	Msg    string    `json:"msg"`
}
