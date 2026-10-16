package main

// 掉盘修复：枚举 NVMe 盘，标注掉线盘，并支持单独禁止/恢复指定盘的 D3cold 深睡。
// 背景：部分 NVMe（国产公方案高发）进 D3cold 后唤不醒导致掉盘（CSTS=0xffff）。
// 手段：对该 PCI 设备写 d3cold_allowed=0 —— 只禁 D3cold 深睡（掉盘元凶），
// 浅层省电（PS3/D3hot）保留，盘仍可低功耗待机，随时可醒。
// 持久化：udev 规则 /etc/udev/rules.d/99-xbox-nvme-keepawake.rules（开机自动生效）。

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DiskSleepEntry 一块 NVMe 盘的状态与深睡配置。
type DiskSleepEntry struct {
	Name       string `json:"name"`       // nvme1
	Model      string `json:"model"`      // 型号
	SizeText   string `json:"sizeText"`   // 238.5G
	PCIAddr    string `json:"pciAddr"`    // 0000:02:00.0
	State      string `json:"state"`      // live / new / dead / unknown
	Down       bool   `json:"down"`       // 是否已掉线（掉线的盘要醒目标出）
	D3coldOK   string `json:"d3coldOk"`   // d3cold_allowed 当前值：1=允许深睡 / 0=已禁深睡 / —
	DeepBanned bool   `json:"deepBanned"` // 本工具是否已禁其 D3cold（udev 规则中存在）
	Mount      string `json:"mount"`      // 首个挂载点（尽力）
}

// nvmeKeepAwakeRule udev 规则文件路径。
const nvmeKeepAwakeRule = "/etc/udev/rules.d/99-xbox-nvme-keepawake.rules"

var rePCIAddr = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-9a-f]$`)

func readSmallStr(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// nvmePCIAddr 取 NVMe 控制器的 PCI 地址（/sys/class/nvme/nvme1/device → 0000:02:00.0）。
func nvmePCIAddr(name string) string {
	link, err := filepath.EvalSymlinks("/sys/class/nvme/" + name + "/device")
	if err != nil {
		return ""
	}
	return filepath.Base(link)
}

func fmtBytesIEC(n int64) string {
	if n <= 0 {
		return "—"
	}
	const u = 1024.0
	f := float64(n)
	units := []string{"B", "K", "M", "G", "T"}
	i := 0
	for f >= u && i < len(units)-1 {
		f /= u
		i++
	}
	return strconv.FormatFloat(f, 'f', 1, 64) + units[i]
}

func collectDiskSleep() []DiskSleepEntry {
	var out []DiskSleepEntry
	kept := readKeptPCI() // 规则文件中已禁深睡的 PCI 地址集合
	dirs, _ := filepath.Glob("/sys/class/nvme/nvme*")
	sort.Strings(dirs)
	for _, d := range dirs {
		name := filepath.Base(d)
		if !isNvmeController(name) {
			continue // 只认 nvme0/nvme1 这类控制器
		}
		e := DiskSleepEntry{Name: name, State: "unknown"}
		e.Model = readSmallStr(filepath.Join(d, "model"))
		e.PCIAddr = nvmePCIAddr(name)
		e.State = readSmallStr(filepath.Join(d, "state"))
		// 块设备容量（/sys/class/block/<name>n1/size，512B 扇区）
		if sz := readSmallStr("/sys/class/block/" + name + "n1/size"); sz != "" {
			if n, err := strconv.ParseInt(sz, 10, 64); err == nil {
				e.SizeText = fmtBytesIEC(n * 512)
			}
		}
		// 挂载点（尽力）
		for _, dk := range listDisks() {
			if baseDiskDevice(dk.Device) == name {
				e.Mount = dk.Mount
				break
			}
		}
		// d3cold_allowed：1=允许深睡 / 0=已禁深睡（浅层省电仍保留）
		if e.PCIAddr != "" {
			e.D3coldOK = readSmallStr("/sys/bus/pci/devices/" + e.PCIAddr + "/d3cold_allowed")
			e.DeepBanned = kept[e.PCIAddr]
		}
		if e.D3coldOK == "" {
			e.D3coldOK = "—"
		}
		// 掉线判定：控制器非 live、容量读不到、或 PCI 电源状态卡在 D3cold
		if e.State != "live" || e.SizeText == "" || e.SizeText == "—" {
			e.Down = true
		}
		if e.PCIAddr != "" {
			if ps := readSmallStr("/sys/bus/pci/devices/" + e.PCIAddr + "/power_state"); strings.Contains(ps, "D3cold") {
				e.Down = true
			}
		}
		out = append(out, e)
	}
	return out
}

// isNvmeController 排除 /sys/class/nvme 下的非控制器条目（nvme1n1 块设备不会出现在这，兜底）。
func isNvmeController(name string) bool {
	return regexp.MustCompile(`^nvme\d+$`).MatchString(name)
}

// readKeptPCI 解析 udev 规则文件中已配置禁深睡的 PCI 地址。
func readKeptPCI() map[string]bool {
	out := map[string]bool{}
	b, err := os.ReadFile(nvmeKeepAwakeRule)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		if i := strings.Index(line, `KERNEL=="`); i >= 0 {
			rest := line[i+len(`KERNEL=="`):]
			if j := strings.Index(rest, `"`); j > 0 {
				out[rest[:j]] = true
			}
		}
	}
	return out
}

// writeKeepAwake 全量重写 udev 规则文件（pci→enabled，只禁 D3cold，浅层省电保留）。
func writeKeepAwake(m map[string]bool) error {
	var sb strings.Builder
	sb.WriteString("# X-Box 掉盘修复：禁止指定 NVMe 的 D3cold 深睡（D3cold 唤不醒导致掉盘）\n")
	sb.WriteString("# 只禁 D3cold，浅层省电（PS3/D3hot）保留；由 X-Box 生成，删除本文件即全部恢复默认\n")
	for pci, en := range m {
		if en {
			sb.WriteString(`ACTION=="add", SUBSYSTEM=="pci", KERNEL=="` + pci + `", ATTR{d3cold_allowed}="0"` + "\n")
		}
	}
	if err := os.WriteFile(nvmeKeepAwakeRule, []byte(sb.String()), 0644); err != nil {
		return err
	}
	// 让 udev 重载规则（不影响运行中的设备，运行态由 API 即时写 d3cold_allowed）
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "udevadm", "control", "--reload-rules").Run()
	return nil
}

// handleNVMeSleep GET 列表。
func handleNVMeSleepList(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]any{"disks": collectDiskSleep()})
}

// handleNVMeSleepSet POST {pci, enable}：单独禁用/恢复指定盘深睡。
func handleNVMeSleepSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PCI    string `json:"pci"`
		Enable bool   `json:"enable"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !rePCIAddr.MatchString(req.PCI) {
		fail(w, 400, "参数错误")
		return
	}
	// 该 PCI 地址必须是一块真实 NVMe 控制器（防误写其它设备）
	found := false
	dirs, _ := filepath.Glob("/sys/class/nvme/nvme*")
	for _, d := range dirs {
		if nvmePCIAddr(filepath.Base(d)) == req.PCI {
			found = true
			break
		}
	}
	if !found {
		fail(w, 400, "未找到该 NVMe 控制器")
		return
	}
	ctl := "/sys/bus/pci/devices/" + req.PCI + "/d3cold_allowed"
	// 1) 即时生效：写 d3cold_allowed（1=允许 D3cold 深睡；0=禁止深睡、保留浅层省电）
	val := "1"
	if req.Enable {
		val = "0"
	}
	if err := os.WriteFile(ctl, []byte(val), 0644); err != nil {
		fail(w, 500, "写入深睡开关失败: "+err.Error())
		return
	}
	// 2) 持久化：更新 udev 规则（重启后依旧生效）
	kept := readKeptPCI()
	kept[req.PCI] = req.Enable
	if err := writeKeepAwake(kept); err != nil {
		fail(w, 500, "写入持久化规则失败: "+err.Error())
		return
	}
	msg := "已恢复该盘 D3cold 深睡（浅层省电本就保留）"
	if req.Enable {
		msg = "已禁止该盘 D3cold 深睡（浅层省电保留，重启后仍生效）"
	}
	ok(w, map[string]any{"message": msg})
}
