package main

// 应用配置导入/导出：系统设置页一键备份与还原。
//
// 范围（仅纯配置，不含可再生运行数据）：
//   - ddns.json            DDNS 服务商（含凭据）与解析记录
//   - cloud-sync.json      网盘同步任务
//   - speedtest_nodes.json 自定义测速节点
//   - media.json           媒体整理任务
//
// 测速历史 / 远端节点缓存 / NAT 检测结果属运行数据，重启可自动再生，不纳入备份。
// 导入语义为「整体替换」：备份里存在的配置项覆盖当前对应配置；缺失项保持不变。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

const cfgImportMaxBytes = 1 << 20 // 导入请求体上限 1MB（实际配置 <100KB）

// cfgExportDoc 导出文档结构（同时作为导入的输入格式）
type cfgExportDoc struct {
	App        string                     `json:"app"`
	Version    string                     `json:"version"`
	ExportedAt string                     `json:"exportedAt"`
	Configs    map[string]json.RawMessage `json:"configs"`
}

// handleConfigExport GET /api/config/export — 聚合各配置文件为单一 JSON。
// 从磁盘读取（各模块每次变更都即时落盘，磁盘即最新真身）；损坏/缺失的文件
// 对应项导出为 null，保证导入端按「存在即导入」处理时不误伤。
func (s *Server) handleConfigExport(w http.ResponseWriter, r *http.Request) {
	doc := cfgExportDoc{
		App:        "X-Box",
		Version:    version,
		ExportedAt: time.Now().Format(time.RFC3339),
		Configs:    map[string]json.RawMessage{},
	}
	raw := func(name string) json.RawMessage {
		b, err := os.ReadFile(filepath.Join(s.dataDir, name))
		if err != nil || len(b) == 0 {
			return nil
		}
		var v any
		if json.Unmarshal(b, &v) != nil {
			return nil // 损坏文件不导出
		}
		return json.RawMessage(b)
	}
	doc.Configs["ddns"] = raw("ddns.json")
	doc.Configs["cloudSync"] = raw("cloud-sync.json")
	doc.Configs["speedtestNodes"] = raw("speedtest_nodes.json")
	doc.Configs["media"] = raw("media.json")
	ok(w, map[string]any{"app": doc.App, "version": doc.Version, "exportedAt": doc.ExportedAt, "configs": doc.Configs})
}

// handleConfigImport POST /api/config/import — 校验后逐项落盘并热更新内存状态。
func (s *Server) handleConfigImport(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, cfgImportMaxBytes))
	if err != nil {
		fail(w, 400, "读取请求失败")
		return
	}
	var req struct {
		Configs map[string]json.RawMessage `json:"configs"`
	}
	if json.Unmarshal(body, &req) != nil || req.Configs == nil {
		fail(w, 400, "配置格式错误：缺少 configs 字段")
		return
	}
	imported := map[string]bool{}
	// DDNS（含服务商凭据）
	if raw, has := req.Configs["ddns"]; has && len(raw) > 0 {
		var cfg DDNSConfig
		if json.Unmarshal(raw, &cfg) != nil {
			fail(w, 400, "DDNS 配置解析失败")
			return
		}
		applyDDNSImport(cfg)
		imported["ddns"] = true
	}
	// 网盘同步任务（调度器每轮从磁盘重读，落盘即热生效）
	if raw, has := req.Configs["cloudSync"]; has && len(raw) > 0 {
		var tasks []SyncTask
		if json.Unmarshal(raw, &tasks) != nil {
			fail(w, 400, "网盘同步配置解析失败")
			return
		}
		if err := saveSyncTasks(tasks); err != nil {
			fail(w, 500, "网盘同步配置写入失败："+err.Error())
			return
		}
		imported["cloudSync"] = true
	}
	// 自定义测速节点
	if raw, has := req.Configs["speedtestNodes"]; has && len(raw) > 0 {
		var list []stNode
		if json.Unmarshal(raw, &list) != nil {
			fail(w, 400, "测速节点配置解析失败")
			return
		}
		applySpeedtestNodesImport(list)
		imported["speedtestNodes"] = true
	}
	// 媒体管理任务
	if raw, has := req.Configs["media"]; has && len(raw) > 0 {
		var list []MediaTask
		if json.Unmarshal(raw, &list) != nil {
			fail(w, 400, "媒体管理配置解析失败")
			return
		}
		if err := applyMediaImport(list); err != nil {
			fail(w, 500, "媒体管理配置写入失败："+err.Error())
			return
		}
		imported["media"] = true
	}
	if len(imported) == 0 {
		fail(w, 400, "备份中没有任何可导入的配置项")
		return
	}
	ok(w, map[string]any{"imported": imported})
}

// applyDDNSImport 用导入配置整体替换当前 DDNS 配置，落盘并触发一轮同步。
func applyDDNSImport(cfg DDNSConfig) {
	if cfg.IntervalSec < 30 {
		cfg.IntervalSec = 30
	}
	var next uint
	for _, p := range cfg.Providers {
		if p.ID > next {
			next = p.ID
		}
	}
	for _, rec := range cfg.Records {
		if rec.ID > next {
			next = rec.ID
		}
	}
	ddns.mu.Lock()
	ddns.cfg = cfg
	ddns.nextID = next
	_ = ddns.saveLocked()
	ddns.mu.Unlock()
	ddns.trigger()
}

// applySpeedtestNodesImport 替换自定义测速节点（补齐缺省 ID/Source）并落盘。
func applySpeedtestNodesImport(list []stNode) {
	now := time.Now().UnixNano()
	for i := range list {
		if list[i].ID == "" {
			list[i].ID = "cu-" + fmt.Sprintf("%d", now%100000) + fmt.Sprintf("%d", i)
		}
		if list[i].Source == "" {
			list[i].Source = "custom"
		}
	}
	stN.mu.Lock()
	stN.custom = list
	stN.saveCustomLocked()
	stN.mu.Unlock()
}
