package main

// 硬件温度采集：CPU（hwmon / thermal_zone）与硬盘（hwmon / smartctl）。
// 全部尽力而为：传感器或 smartctl 缺失时返回空，前端显示「—」，不影响其他功能。

import (
	"bufio"
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// TempSensor 单点温度读数（CPU）。
type TempSensor struct {
	Label string  `json:"label"` // 如 "Package id 0" / "Core 0" / "x86_pkg_temp"
	TempC float64 `json:"tempC"`
}

// DiskTemp 单块硬盘温度。
type DiskTemp struct {
	Device string  `json:"device"`           // sda / nvme0n1（已去分区后缀）
	Model  string  `json:"model,omitempty"`  // 型号（尽力获取）
	Mount  string  `json:"mount,omitempty"`  // 该盘首个挂载点（若有）
	TempC  float64 `json:"tempC"`            // 0 = 未知
	Source string  `json:"source,omitempty"` // hwmon(<name>) / smartctl
	Note   string  `json:"note,omitempty"`   // 探测失败原因（前端悬浮提示，便于远程排障）
}

// TempsInfo 温度汇总（概览页用）。
type TempsInfo struct {
	CPU    []TempSensor `json:"cpu"`
	CPUMax float64      `json:"cpuMax"` // 0 = 未知（Package 优先，否则取最高）
	Disks  []DiskTemp   `json:"disks"`
}

func readSmall(p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// readCPUTemps 采集 CPU 相关温度传感器。
// 优先 hwmon（coretemp/k10temp 等，带 label），无结果回退 thermal_zone。
// 过滤明显异常值（<=0 或 >=150℃）。
func readCPUTemps() []TempSensor {
	var out []TempSensor
	dirs, _ := filepath.Glob("/sys/class/hwmon/hwmon*")
	for _, d := range dirs {
		dev := strings.ToLower(readSmall(filepath.Join(d, "name")))
		inputs, _ := filepath.Glob(filepath.Join(d, "temp*_input"))
		for _, in := range inputs {
			v, err := strconv.ParseFloat(readSmall(in), 64)
			if err != nil || v <= 0 || v >= 150000 {
				continue
			}
			if !isCPUSensor(dev, in) {
				continue
			}
			label := readSmall(strings.TrimSuffix(in, "_input") + "_label")
			if label == "" {
				label = dev
			}
			out = append(out, TempSensor{Label: label, TempC: v / 1000})
		}
	}
	if len(out) == 0 {
		// 回退 1：thermal_zone 里 type 明确属于 CPU 的（x86_pkg_temp / cpu-thermal / soc）
		zones, _ := filepath.Glob("/sys/class/thermal/thermal_zone*")
		type zoneVal struct {
			typ string
			val float64
		}
		var valid []zoneVal
		for _, z := range zones {
			typ := strings.ToLower(readSmall(filepath.Join(z, "type")))
			v, err := strconv.ParseFloat(readSmall(filepath.Join(z, "temp")), 64)
			if err != nil || v <= 0 || v >= 150000 {
				continue
			}
			if strings.Contains(typ, "cpu") || strings.Contains(typ, "pkg") ||
				strings.Contains(typ, "x86") || strings.Contains(typ, "soc") {
				out = append(out, TempSensor{Label: typ, TempC: v / 1000})
			}
			valid = append(valid, zoneVal{typ, v / 1000})
		}
		// 回退 2：没有任何 CPU 类型的热区（部分设备只暴露 acpitz 等），
		// 取有效热区中的最高值作为参考值，并在标签上标明来源，
		// 让界面显示一个可用的温度而不是"—"。
		if len(out) == 0 && len(valid) > 0 {
			best := valid[0]
			for _, z := range valid[1:] {
				if z.val > best.val {
					best = z
				}
			}
			out = append(out, TempSensor{Label: best.typ + " (参考)", TempC: best.val})
		}
	}
	return out
}

// isCPUSensor 判断 hwmon 传感器是否属于 CPU（排除 acpitz 主板 / nvme / 硬盘等）。
func isCPUSensor(dev, inputPath string) bool {
	cpuDevs := []string{"coretemp", "k10temp", "k8temp", "cpu_thermal", "zenpower", "fam15h_power", "cpu"}
	for _, c := range cpuDevs {
		if dev == c {
			return true
		}
	}
	// 兜底：label 含 cpu/core/package/tctl/tdie
	l := strings.ToLower(readSmall(strings.TrimSuffix(inputPath, "_input") + "_label"))
	return strings.Contains(l, "cpu") || strings.Contains(l, "core") ||
		strings.Contains(l, "package") || strings.Contains(l, "tctl") || strings.Contains(l, "tdie")
}

// summarizeCPUTemps 概览统计卡用温度：有 Package 取 Package，否则取最高。
func summarizeCPUTemps(sensors []TempSensor) float64 {
	if len(sensors) == 0 {
		return 0
	}
	var pkg float64
	for _, s := range sensors {
		if strings.Contains(strings.ToLower(s.Label), "package") && s.TempC > pkg {
			pkg = s.TempC
		}
	}
	if pkg > 0 {
		return pkg
	}
	mx := sensors[0].TempC
	for _, s := range sensors {
		if s.TempC > mx {
			mx = s.TempC
		}
	}
	return mx
}

// baseDiskDevice 块设备名去分区后缀：sda2→sda，nvme0n1p3→nvme0n1，mmcblk0p1→mmcblk0。
func baseDiskDevice(dev string) string {
	base := filepath.Base(dev)
	if strings.HasPrefix(base, "nvme") || strings.HasPrefix(base, "mmcblk") {
		if i := strings.Index(base, "p"); i > 0 {
			return base[:i]
		}
		return base
	}
	i := len(base)
	for i > 0 && base[i-1] >= '0' && base[i-1] <= '9' {
		i--
	}
	if i == 0 {
		return base
	}
	return base[:i]
}

// readDiskModel 尽力读取硬盘型号。
func readDiskModel(dev string) string {
	m := readSmall(filepath.Join("/sys/class/block", dev, "device", "model"))
	if m == "" {
		m = readSmall(filepath.Join("/sys/class/block", dev, "device", "device", "model"))
	}
	return m
}

// modprobeDrivetemp 一次性尝试加载 drivetemp 内核模块：加载后 SATA 盘温度会以
// hwmon（/sys/class/block/<dev>/device/hwmon/...）形式出现，fnOS 未装 smartctl 时的兜底。
var modprobeOnce sync.Once

func tryModprobeDrivetemp() {
	modprobeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "modprobe", "drivetemp").Run()
	})
}

// hwmonTempFor 通用 hwmon 匹配：按 device 符号链接的真实路径对齐，
// 而非假定 hwmon 挂在固定嵌套路径下——NVMe 的 hwmon 在控制器（nvme0）上，
// drivetemp 的在 ata_device 上，不同内核/驱动挂载点不一致。
func hwmonTempFor(dev string) (float64, string) {
	devReal, err := filepath.EvalSymlinks("/sys/class/block/" + dev + "/device")
	if err != nil {
		return 0, ""
	}
	dirs, _ := filepath.Glob("/sys/class/hwmon/hwmon*")
	for _, d := range dirs {
		hd, err := filepath.EvalSymlinks(filepath.Join(d, "device"))
		if err != nil || hd != devReal {
			continue
		}
		name := readSmall(filepath.Join(d, "name"))
		ins, _ := filepath.Glob(filepath.Join(d, "temp*_input"))
		for _, in := range ins {
			if v, err := strconv.ParseFloat(readSmall(in), 64); err == nil && v > 0 && v < 150000 {
				return v / 1000, name
			}
		}
	}
	return 0, ""
}

// readDiskTemp 读取单盘温度：hwmon（NVMe / drivetemp，按 realpath 对齐）→ smartctl（SATA）。
func readDiskTemp(dev string) (tempC float64, source, model, note string) {
	model = readDiskModel(dev)
	// 0) drivetemp 模块兜底（SATA 盘 hwmon），只尝试一次
	tryModprobeDrivetemp()
	// 1) 通用 hwmon 匹配
	if t, name := hwmonTempFor(dev); t > 0 {
		return t, "hwmon(" + name + ")", model, ""
	}
	// 2) 旧固定路径兜底（个别内核 hwmon 不在 device 链上）
	if ins, _ := filepath.Glob("/sys/class/block/" + dev + "/device/hwmon/hwmon*/temp1_input"); len(ins) > 0 {
		for _, in := range ins {
			if v, err := strconv.ParseFloat(readSmall(in), 64); err == nil && v > 0 && v < 150000 {
				return v / 1000, "hwmon", model, ""
			}
		}
	}
	// 3) smartctl：-n standby 避免唤醒休眠盘
	if _, err := exec.LookPath("smartctl"); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "smartctl", "-A", "-n", "standby", "/dev/"+dev).Output()
		if err == nil {
			if t, ok := parseSmartTemp(string(out)); ok {
				return t, "smartctl", model, ""
			}
			return 0, "", model, "smartctl 输出无温度属性"
		}
		return 0, "", model, "smartctl 执行失败: " + err.Error()
	}
	return 0, "", model, "无 hwmon 传感器且 smartctl 未安装"
}

// parseSmartTemp 从 smartctl -A 输出解析温度（属性 194 Temperature_Celsius / 190 Airflow_Temperature_Cel）。
func parseSmartTemp(out string) (float64, bool) {
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, "Temperature_Celsius") && !strings.Contains(line, "Airflow_Temperature_Cel") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 10 {
			continue
		}
		// SMART 属性行：ID NAME FLAG VALUE WORST THRESH TYPE UPDATED WHEN_FAILED RAW_VALUE…
		// RAW_VALUE 首 token 即温度（后续可能是 "(Min/Max 20/45)" 之类）
		if v, err := strconv.ParseFloat(f[9], 64); err == nil && v > 0 && v < 150 {
			return v, true
		}
		if v, err := strconv.ParseFloat(f[len(f)-1], 64); err == nil && v > 0 && v < 150 {
			return v, true
		}
	}
	return 0, false
}

// rePhysDisk 物理整盘设备名：sda / vd b / hda / nvme0n1 / mmcblk0（排除分区 sda1、nvme0n1p3）。
var rePhysDisk = regexp.MustCompile(`^(sd[a-z]+|vd[a-z]+|hd[a-z]+|nvme\d+n\d+|mmcblk\d+)$`)

// collectTemps 采集 CPU 与硬盘温度。硬盘并发探测，单盘 smartctl 超时 3s。
// 硬盘遍历 /sys/class/block 全量物理盘而非挂载表：fnOS 存储空间常为 md RAID
// （挂载设备是 md0），只有遍历物理盘才能读到每块盘（含 RAID 从属盘）的温度。
func collectTemps() TempsInfo {
	info := TempsInfo{}
	info.CPU = readCPUTemps()
	info.CPUMax = summarizeCPUTemps(info.CPU)

	// 挂载点映射（base 设备名 → 首个挂载点），尽力填充展示信息
	mounts := map[string]string{}
	for _, d := range listDisks() {
		dev := baseDiskDevice(d.Device)
		if _, ok := mounts[dev]; !ok {
			mounts[dev] = d.Mount
		}
	}

	type job struct{ dev, mount string }
	var jobs []job
	entries, err := os.ReadDir("/sys/class/block")
	if err == nil {
		for _, e := range entries {
			name := e.Name()
			if !rePhysDisk.MatchString(name) {
				continue
			}
			jobs = append(jobs, job{name, mounts[name]})
		}
	}
	if len(jobs) == 0 {
		// 极端情况兜底：/sys/class/block 读不了时退回挂载表
		seen := map[string]bool{}
		for _, d := range listDisks() {
			dev := baseDiskDevice(d.Device)
			if dev == "" || seen[dev] {
				continue
			}
			seen[dev] = true
			jobs = append(jobs, job{dev, d.Mount})
		}
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, j := range jobs {
		wg.Add(1)
		go func(dev, mount string) {
			defer wg.Done()
			t, src, model, note := readDiskTemp(dev)
			mu.Lock()
			info.Disks = append(info.Disks, DiskTemp{Device: dev, Model: model, Mount: mount, TempC: t, Source: src, Note: note})
			mu.Unlock()
		}(j.dev, j.mount)
	}
	wg.Wait()
	sort.Slice(info.Disks, func(i, j int) bool { return info.Disks[i].Device < info.Disks[j].Device })
	return info
}

// 温度缓存：/api/temps 被 5s 轮询，smartctl 探测有成本，3s 内沿用上次结果。
var (
	tempsMu      sync.Mutex
	tempsCache   TempsInfo
	tempsCacheAt time.Time
)

func collectTempsCached() TempsInfo {
	tempsMu.Lock()
	defer tempsMu.Unlock()
	if !tempsCacheAt.IsZero() && time.Since(tempsCacheAt) < 3*time.Second {
		return tempsCache
	}
	t := collectTemps()
	tempsCache, tempsCacheAt = t, time.Now()
	return t
}

// handleTemps 轻量温度接口（前端 5s 轮询局部刷新，比整页 overview 便宜得多）。
func handleTemps(w http.ResponseWriter, r *http.Request) {
	// 与 /api/overview 里的 temps 字段保持同构（扁平：cpu / cpuMax / disks）。
	// 之前包了一层 {temps:{...}}，而前端轮询后按顶层取 t.disks，导致
	// "首次进入有温度、5 秒后变成暂无传感器"。
	t := collectTempsCached()
	ok(w, map[string]any{"cpu": t.CPU, "cpuMax": t.CPUMax, "disks": t.Disks})
}
