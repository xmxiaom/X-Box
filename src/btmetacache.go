package main

// 元数据磁盘缓存（v1.3.43）：抓到的 ut_metadata 原文按 infohash 存盘，
// 同一个种子再次下载（重试 / 重复提交 / 进程重启后恢复）直接读缓存，
// 秒进下载态，不再走「拨 20 多个 peer 抓 20 多个分片」的漫长过程。
//
// 设计要点：
//   - 键 = infohash hex，值 = info dict 的 bencode 原文（不含外层 d...e）；
//   - 落盘前用 btParseMetaFromRaw 复核（SHA1(info)==infohash），缓存损坏不影响正确性；
//   - 只缓存小于 btMaxMetaSize 的元数据，目录失败静默降级（缓存是加速项，不是必需项）；
//   - 上限 200 个文件，超出按修改时间删最旧（防止长期运行把磁盘塞满）。

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const btMetaCacheMax = 200

var btMetaCache struct {
	mu   sync.Mutex
	dir  string
	once sync.Once
}

func btMetaCacheDir() string {
	btMetaCache.once.Do(func() {
		base := os.Getenv("XBOX_DATA")
		if base == "" {
			base = "."
		}
		btMetaCache.dir = filepath.Join(base, "torrent-meta")
	})
	return btMetaCache.dir
}

// btMetaCachePath infohash → 缓存文件路径
func btMetaCachePath(ih [20]byte) string {
	return filepath.Join(btMetaCacheDir(), btHashHex(ih)+".bin")
}

// btMetaCacheGet 读缓存并复核；无缓存/损坏返回 nil
func btMetaCacheGet(ih [20]byte) *btMeta {
	raw, err := os.ReadFile(btMetaCachePath(ih))
	if err != nil || len(raw) == 0 || int64(len(raw)) > btMaxMetaSize {
		return nil
	}
	meta, err := btParseMetaFromRaw(raw, ih)
	if err != nil {
		os.Remove(btMetaCachePath(ih)) // 损坏即清，避免反复读坏
		return nil
	}
	return meta
}

// btMetaCachePut 写缓存（先复核再落盘，异步清理旧文件）
func btMetaCachePut(ih [20]byte, meta *btMeta) {
	if meta == nil || meta.raw == nil || int64(len(meta.raw)) > btMaxMetaSize {
		return
	}
	dir := btMetaCacheDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	tmp := filepath.Join(dir, fmt.Sprintf(".%s.tmp%d", btHashHex(ih), time.Now().UnixNano()%1e6))
	if err := os.WriteFile(tmp, meta.raw, 0o644); err != nil {
		os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, btMetaCachePath(ih)); err != nil {
		os.Remove(tmp)
		return
	}
	go btMetaCacheTrim()
}

// btMetaCacheTrim 只保留最近 btMetaCacheMax 个缓存
func btMetaCacheTrim() {
	defer func() { recover() }()
	btMetaCache.mu.Lock()
	defer btMetaCache.mu.Unlock()
	ents, err := os.ReadDir(btMetaCacheDir())
	if err != nil || len(ents) <= btMetaCacheMax {
		return
	}
	type fi struct {
		name string
		mod  time.Time
	}
	list := make([]fi, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".bin") {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		list = append(list, fi{name: e.Name(), mod: info.ModTime()})
	}
	if len(list) <= btMetaCacheMax {
		return
	}
	sort.Slice(list, func(a, b int) bool { return list[a].mod.After(list[b].mod) })
	for _, f := range list[btMetaCacheMax:] {
		os.Remove(filepath.Join(btMetaCacheDir(), f.name))
	}
}
