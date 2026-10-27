package main

// 搜索结果留存（v1.3.49）：把最近一次搜索的响应落盘到 XBOX_DATA，
// 重开应用/刷新页面后前端自动恢复展示，不用重新搜一遍。
// 纯展示缓存：不参与下载逻辑；写盘用临时文件+rename 防半截文件。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

const dlSearchLastMax = 4 << 20 // 单份缓存上限 4MB（正常几百 KB 内）

func dlSearchLastPath(kind string) string {
	base := os.Getenv("XBOX_DATA")
	if base == "" {
		base = "."
	}
	return filepath.Join(base, "search-last-"+kind+".json")
}

// dlSearchLastSave 保存最近一次搜索结果（kind: video|music）
func dlSearchLastSave(kind string, payload map[string]any) {
	payload["kind"] = kind
	payload["at"] = time.Now().Format(time.RFC3339)
	b, err := json.Marshal(payload)
	if err != nil || len(b) > dlSearchLastMax {
		return
	}
	p := dlSearchLastPath(kind)
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, p)
	}
}

// dlSearchLastLoad 读最近一次搜索结果；无/损坏返回 nil
func dlSearchLastLoad(kind string) map[string]any {
	b, err := os.ReadFile(dlSearchLastPath(kind))
	if err != nil || len(b) == 0 || len(b) > dlSearchLastMax {
		return nil
	}
	var v map[string]any
	if json.Unmarshal(b, &v) != nil {
		return nil
	}
	return v
}
