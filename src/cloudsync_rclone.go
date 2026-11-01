package main

// 刷新网盘挂载目录的缓存（唯一实现：rclone rc vfs/forget）。
//
// 依据 rclone 官方文档：
//   - VFS 层会缓存目录树，在 --dir-cache-time 内不会重新向远端列目录；
//   - 官方提供 rc 接口 vfs/forget 精确丢弃某个目录的缓存（不带参数则清空全部），
//     这是「让目录立刻可见」的正规手段；
//   - 飞牛「远程挂载」= 网盘转本机 WebDAV + rclone mount，配置在
//     /etc/mountmgr/rclone/*.conf，rc 监听默认端口 5572（社区用 /mount/unmount、
//     /vfs/stats 操作过），因此可直接调用。
//
// 这里只做这一件事：调 vfs/forget 丢弃目录缓存。
// 不做 SIGHUP、不做逐目录 readdir 之类无效的替代尝试。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	rcloneMu     sync.Mutex
	rcloneLastAt time.Time
	rcloneLastOK bool
	rcloneNote   string
)

// rcloneRCAddr rc 地址：默认 rclone 标准端口，可用 XBOX_RCLONE_RC 覆盖
func rcloneRCAddr() string {
	if v := strings.TrimSpace(os.Getenv("XBOX_RCLONE_RC")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://127.0.0.1:5572"
}

func rcloneRC(endpoint string, payload map[string]any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	cl := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest("POST", rcloneRCAddr()+endpoint, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != 200 {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 160 {
			msg = msg[:160]
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
	}
	var out map[string]any
	if json.Unmarshal(body, &out) == nil {
		if e, ok := out["error"].(string); ok && e != "" {
			return fmt.Errorf("%s", e)
		}
	}
	return nil
}

// vfsForget 丢弃指定目录（挂载内相对路径）的缓存；dir 为空表示全部
func vfsForget(dir string) error {
	if dir == "" {
		return rcloneRC("/vfs/forget", nil)
	}
	return rcloneRC("/vfs/forget", map[string]any{"dir": dir})
}

// refreshMountCache 让 rclone 丢弃全部目录缓存（官方 vfs/forget 的默认语义）。
// 不按目录传参：rclone 对不存在的路径同样返回成功，无法确认是否真的清到，
// 而整棵清空是文档里语义最明确的做法，代价仅是各挂载下次访问重新列一次目录。
// 节流 5 秒：实时模式 1~2 秒一轮，没必要每轮都触发。
func refreshMountCache() bool {
	rcloneMu.Lock()
	defer rcloneMu.Unlock()
	if time.Since(rcloneLastAt) < 5*time.Second {
		return rcloneLastOK
	}
	rcloneLastAt = time.Now()

	if err := rcloneRC("/vfs/forget", nil); err != nil {
		if rcloneLastOK || rcloneNote != err.Error() {
			log.Printf("[同步] 目录缓存刷新失败：%v", err)
			rcloneNote = err.Error()
		}
		rcloneLastOK = false
		return false
	}
	if !rcloneLastOK {
		log.Printf("[同步] 已通过 rclone rc 丢弃目录缓存")
	}
	rcloneNote = ""
	rcloneLastOK = true
	return true
}
