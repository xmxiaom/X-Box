package main

// btmetadiag.go 开发专用诊断入口（只在 XBOX_META_DIAG 环境变量下生效，正常构建不影响）。
// 直接调用 btFetchMetaFromPeer（与引擎完全同一个函数、同一进程环境），
// 按 12 并发波次对 peers 文件逐个抓元数据，逐行打印结果。
// 用法: XBOX_META_DIAG=peers.txt XBOX_META_IH=<40位hex> XBOX_DEV=1 x-box.exe
import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

func btMetaDiagMain(path string) {
	ihs, _ := os.LookupEnv("XBOX_META_IH")
	ihRaw, err := hexDecode40(ihs)
	if err != nil {
		fmt.Println("XBOX_META_IH 无效:", err)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		fmt.Println("open peers:", err)
		return
	}
	defer f.Close()
	var addrs []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		s := strings.TrimSpace(sc.Text())
		if s != "" {
			addrs = append(addrs, s)
		}
	}
	conc := 12
	if v, err := strconv.Atoi(os.Getenv("XBOX_META_DIAG_CONC")); err == nil && v > 0 {
		conc = v
	}
	fmt.Printf("[diag] %d peers conc=%d ih=%x\n", len(addrs), conc, ihRaw)
	for wave := 0; wave*conc < len(addrs); wave++ {
		batch := addrs[wave*conc:]
		if len(batch) > conc {
			batch = batch[:conc]
		}
		fmt.Printf("[diag] === wave %d ===\n", wave+1)
		var wg sync.WaitGroup
		var mu sync.Mutex
		for _, a := range batch {
			wg.Add(1)
			go func(a string) {
				defer wg.Done()
				t0 := time.Now()
				ctx, cancel := context.WithTimeout(context.Background(), btMetaPeerCap)
				m, err := btFetchMetaFromPeer(ctx, a, ihRaw)
				d := time.Since(t0).Seconds()
				cancel()
				mu.Lock()
				if err != nil {
					fmt.Printf("[diag] %s FAIL %.1fs %v\n", a, d, err)
				} else {
					fmt.Printf("[diag] %s OK   %.1fs size=%d files=%d\n", a, d, m.Len, len(m.Files))
				}
				mu.Unlock()
			}(a)
		}
		wg.Wait()
	}
	fmt.Println("[diag] done")
}

func hexDecode40(s string) ([20]byte, error) {
	var out [20]byte
	if len(s) != 40 {
		return out, fmt.Errorf("长度 %d != 40", len(s))
	}
	for i := 0; i < 20; i++ {
		v, err := strconv.ParseUint(s[i*2:i*2+2], 16, 8)
		if err != nil {
			return out, err
		}
		out[i] = byte(v)
	}
	return out, nil
}

// btMetaDiagParsePeerLine 预留：容错拆 host:port（诊断用）
func btMetaDiagParsePeerLine(s string) (string, bool) {
	h, p, err := net.SplitHostPort(s)
	if err != nil {
		return "", false
	}
	return net.JoinHostPort(h, p), true
}
