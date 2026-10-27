package main

// 开机启动项扫描：systemd 服务/定时器、rc.local、cron @reboot、init.d、fnOS 应用。

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type AutostartItem struct {
	Name    string `json:"name"`
	Type    string `json:"type"`  // systemd-service / systemd-timer / rc-local / cron / initd / fnos-app
	State   string `json:"state"` // enabled / disabled / running / n/a
	Source  string `json:"source"`
	Desc    string `json:"desc"`
	Running bool   `json:"running"`
}

// 自启扫描要遍历 systemd 单元、cron、init.d 等，单次约 1 秒以上（单元多的设备更久）。
// 三级加速保证「秒开」：
//  1. 启动预热：应用起来就后台扫一遍（warmAutostart），用户点进来时缓存已就绪；
//  2. 结果长期缓存（10 分钟 TTL），页内是只读列表，旧数据无一致性风险；
//  3. 过期也立即返回旧数据 + 后台单飞刷新（stale-while-revalidate），任何时刻请求都不阻塞。
var autoCache struct {
	mu         sync.Mutex
	at         time.Time
	data       []AutostartItem
	refreshing bool
}

const autoTTL = 10 * time.Minute

func (s *Server) handleAutostart(w http.ResponseWriter, r *http.Request) {
	autoCache.mu.Lock()
	if autoCache.data != nil && time.Since(autoCache.at) < autoTTL {
		items := autoCache.data
		autoCache.mu.Unlock()
		ok(w, map[string]any{"items": items, "cached": true})
		return
	}
	stale := autoCache.data
	if stale != nil {
		// 过期但有旧数据：立即返回旧数据，后台单飞刷新（防止请求风暴）
		if !autoCache.refreshing {
			autoCache.refreshing = true
			go func() {
				items := listAutostart()
				autoCache.mu.Lock()
				autoCache.at, autoCache.data, autoCache.refreshing = time.Now(), items, false
				autoCache.mu.Unlock()
			}()
		}
		autoCache.mu.Unlock()
		ok(w, map[string]any{"items": stale, "cached": true})
		return
	}
	autoCache.mu.Unlock()

	items := listAutostart() // 首次且无预热数据：同步扫描（正常被启动预热兜住，走不到）
	autoCache.mu.Lock()
	autoCache.at, autoCache.data = time.Now(), items
	autoCache.mu.Unlock()
	ok(w, map[string]any{"items": items})
}

// warmAutostart 应用启动后后台预热一次，让首次点击也是秒开。
func warmAutostart() {
	items := listAutostart()
	autoCache.mu.Lock()
	autoCache.at, autoCache.data = time.Now(), items
	autoCache.mu.Unlock()
}

func listAutostart() []AutostartItem {
	var out []AutostartItem

	// 1+2. systemd 启用的服务与定时器（一次 list-unit-files 全拿，按后缀分拣——省一半扫描时间）
	out = append(out, systemdEnabledAll()...)

	// 3. rc.local
	if fi, err := os.Stat("/etc/rc.local"); err == nil && !fi.IsDir() {
		st := "disabled"
		if fi.Mode()&0111 != 0 {
			st = "enabled"
		}
		out = append(out, AutostartItem{
			Name: "rc.local", Type: "rc-local", State: st,
			Source:  "/etc/rc.local",
			Desc:    "开机脚本（/etc/rc.local），存在且可执行即会于启动末期执行",
		})
	}

	// 4. cron @reboot
	out = append(out, cronReboot()...)

	// 5. init.d 在 rc*.d 有 S 链接
	out = append(out, initdEnabled()...)

	// 6. fnOS 应用（/var/apps 下目录 = 已安装，自启与否由应用中心管理）
	out = append(out, fnosAppsAutostart()...)

	return out
}

func systemdEnabledAll() []AutostartItem {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	// 一次列出全部 enabled 单元（不分 type），按后缀分拣出服务与定时器
	b, err := exec.CommandContext(ctx, "systemctl", "list-unit-files", "--state=enabled", "--no-pager", "--no-legend").Output()
	if err != nil {
		return nil
	}
	var out []AutostartItem
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		name := f[0]
		var t string
		switch {
		case strings.HasSuffix(name, ".service"):
			t = "systemd-service"
		case strings.HasSuffix(name, ".timer"):
			t = "systemd-timer"
		default:
			continue // target/socket/mount 等不属于「开机自启」展示范围
		}
		out = append(out, AutostartItem{
			Name: name, Type: t, State: "enabled",
			Source: "/etc/systemd/system/" + name,
			Desc:    "已启用开机自启（systemctl enable）",
		})
	}
	return out
}

func cronReboot() []AutostartItem {
	var out []AutostartItem
	const marker = "@reboot"
	scan := func(content, src string) {
		for _, line := range strings.Split(content, "\n") {
			l := strings.TrimSpace(line)
			if l == "" || strings.HasPrefix(l, "#") {
				continue
			}
			if strings.Contains(l, marker) {
				out = append(out, AutostartItem{
					Name: "cron@reboot: " + shortCron(l), Type: "cron", State: "enabled",
					Source: src, Desc: "crontab 中 @reboot 条目，开机时执行一次：" + l,
				})
			}
		}
	}
	// root crontab：优先 /var/spool/cron/crontabs/root，再用 crontab -l
	if b, err := os.ReadFile("/var/spool/cron/crontabs/root"); err == nil {
		scan(string(b), "/var/spool/cron/crontabs/root")
	} else if b, err := os.ReadFile("/var/spool/cron/root"); err == nil {
		scan(string(b), "/var/spool/cron/root")
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if b, err := exec.CommandContext(ctx, "crontab", "-l").Output(); err == nil {
			scan(string(b), "root crontab")
		}
		cancel()
	}
	// /etc/cron.d/*
	if entries, err := os.ReadDir("/etc/cron.d"); err == nil {
		for _, e := range entries {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			if b, err := os.ReadFile("/etc/cron.d/" + e.Name()); err == nil {
				scan(string(b), "/etc/cron.d/"+e.Name())
			}
		}
	}
	return out
}

func shortCron(l string) string {
	idx := strings.Index(l, "@reboot")
	rest := strings.TrimSpace(l[idx+len("@reboot"):])
	return rest
}

func initdEnabled() []AutostartItem {
	dirs, _ := filepath.Glob("/etc/rc*.d")
	if len(dirs) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []AutostartItem
	files, _ := filepath.Glob("/etc/init.d/*")
	for _, f := range files {
		name := filepath.Base(f)
		if seen[name] {
			continue
		}
		for _, rd := range dirs {
			links, _ := filepath.Glob(rd + "/S*" + name)
			if len(links) > 0 {
				seen[name] = true
				out = append(out, AutostartItem{
					Name: name, Type: "initd", State: "enabled",
					Source:  f,
					Desc:    "SysV 启动脚本，已在 " + filepath.Base(rd) + " 注册 S 链接，开机执行",
				})
				break
			}
		}
	}
	return out
}

func fnosAppsAutostart() []AutostartItem {
	dirs, _ := filepath.Glob("/var/apps/*")
	var out []AutostartItem
	for _, d := range dirs {
		name := filepath.Base(d)
		if name == "" || strings.HasPrefix(name, ".") {
			continue
		}
		out = append(out, AutostartItem{
			Name:    name, Type: "fnos-app", State: "managed",
			Source:  d,
			Desc:    "fnOS 应用（/var/apps 下），自启策略由飞牛应用中心管理",
		})
	}
	return out
}

// 保留函数引用，避免未使用告警（strconv 用于未来扩展）。
var _ = strconv.Itoa
