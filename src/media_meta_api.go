// 在线元数据客户端：全部使用免密钥的公开 API
//
//	MusicBrainz（https://musicbrainz.org/ws/2）
//	  权威音乐元数据（歌手/专辑/歌名/年份/音轨号），无需 API Key。
//	  硬性要求：必须带可识别的 User-Agent，否则 403；限速 1 请求/秒/IP。
//	iTunes Search（https://itunes.apple.com/search）
//	  免密钥，用于补充封面、时长、专辑；限速约 20 次/分钟。
//	Cover Art Archive（https://coverartarchive.org）
//	  按 MusicBrainz release MBID 取封面，无需密钥。
//	lyrics.ovh（https://api.lyrics.ovh/v1）
//	  免密钥歌词接口，返回纯文本歌词。
//
// 所有请求均带超时与失败降级：在线服务不可用时退回本地启发式结果。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// mediaHTTP 全局客户端（在线元数据）
var mediaHTTP = &http.Client{Timeout: 12 * time.Second}

// mediaUserAgent MusicBrainz 要求可识别的 UA（应用名/版本/联系方式）
const mediaUserAgent = "X-Box-MediaManager/1.0 (https://github.com/xmxiaom/X-Box)"

// musicBrainzLimiter 限速器：保证两次请求间隔 ≥1.1 秒
var musicBrainzLimiter = struct {
	sync.Mutex
	last time.Time
}{}

// mbWait 等待限速窗口
func mbWait() {
	musicBrainzLimiter.Lock()
	defer musicBrainzLimiter.Unlock()
	wait := 1100*time.Millisecond - time.Since(musicBrainzLimiter.last)
	if wait > 0 {
		time.Sleep(wait)
	}
	musicBrainzLimiter.last = time.Now()
}

// ---------- MusicBrainz ----------

// mbRecording 搜索结果
type mbRecording struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Score        int    `json:"score"`
	Length       int    `json:"length"`
	ArtistCredit []struct {
		Name   string `json:"name"`
		Artist struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"artist"`
	} `json:"artist-credit"`
	Releases []struct {
		ID     string `json:"id"`
		Title  string `json:"title"`
		Date   string `json:"date"`
		Status string `json:"status"`
		Media  []struct {
			Format     string `json:"format"`
			TrackCount int    `json:"track-count"`
			Position   int    `json:"position"`
		} `json:"media"`
	} `json:"releases"`
}

// mbSearchResp MusicBrainz 搜索响应
type mbSearchResp struct {
	Recordings []mbRecording `json:"recordings"`
	Count      int           `json:"count"`
}

// mbFetchJSON 发起 MusicBrainz 请求（自动限速 + UA）
func mbFetchJSON(path string, out any) error {
	mbWait()
	req, err := http.NewRequest("GET", "https://musicbrainz.org/ws/2"+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", mediaUserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := mediaHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("MusicBrainz HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// mbSearchRecording 按歌名+歌手搜索录音
// 返回按得分排序的候选列表
func mbSearchRecording(title, artist string, limit int) ([]mbRecording, error) {
	if limit <= 0 || limit > 25 {
		limit = 5
	}
	q := url.QueryEscape("recording:" + title)
	if artist != "" {
		q += " AND " + url.QueryEscape("artist:"+artist)
	}
	path := fmt.Sprintf("/recording?query=%s&fmt=json&limit=%d", q, limit)
	var resp mbSearchResp
	if err := mbFetchJSON(path, &resp); err != nil {
		return nil, err
	}
	return resp.Recordings, nil
}

// mbMetaFromRecording 把 MusicBrainz 录音结果转成 MediaMeta
func mbMetaFromRecording(rec mbRecording) MediaMeta {
	var m MediaMeta
	m.Title = rec.Title
	if len(rec.ArtistCredit) > 0 {
		// 拼接全部 artist-credit（feat. 等）
		names := make([]string, 0, len(rec.ArtistCredit))
		for _, ac := range rec.ArtistCredit {
			if ac.Name != "" {
				names = append(names, ac.Name)
			} else if ac.Artist.Name != "" {
				names = append(names, ac.Artist.Name)
			}
		}
		m.Artist = strings.Join(names, ", ")
	}
	// 选一个 release 作为专辑来源（优先官方发行）
	if len(rec.Releases) > 0 {
		best := rec.Releases[0]
		for _, r := range rec.Releases {
			if r.Status == "Official" {
				best = r
				break
			}
		}
		m.Album = best.Title
		m.Year = parseYear(best.Date)
		for _, med := range best.Media {
			if med.TrackCount > 0 {
				m.TrackTotal = med.TrackCount
			}
			break
		}
	}
	m.Duration = float64(rec.Length) / 1000.0
	return m
}

// mbLookupRelease 取 release 的 MBID（用于封面）
func mbLookupRelease(title, artist string) (string, string) {
	q := url.QueryEscape("release:" + title)
	if artist != "" {
		q += " AND " + url.QueryEscape("artist:"+artist)
	}
	path := fmt.Sprintf("/release?query=%s&fmt=json&limit=3", q)
	var resp struct {
		Releases []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
			Date  string `json:"date"`
		} `json:"releases"`
	}
	if err := mbFetchJSON(path, &resp); err != nil {
		return "", ""
	}
	if len(resp.Releases) == 0 {
		return "", ""
	}
	return resp.Releases[0].ID, resp.Releases[0].Title
}

// mbFetchCover 按 release MBID 取封面（front cover）
func mbFetchCover(mbid string) []byte {
	if mbid == "" {
		return nil
	}
	url := "https://coverartarchive.org/release/" + url.PathEscape(mbid)
	resp, err := mediaHTTP.Get(url)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	var list []struct {
		Front bool     `json:"front"`
		Image string   `json:"image"`
		Thumb string   `json:"thumbnail"`
		Types []string `json:"types"`
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return nil
	}
	for _, it := range list {
		if it.Front {
			return downloadImage(it.Image)
		}
	}
	if len(list) > 0 {
		return downloadImage(list[0].Image)
	}
	return nil
}

// downloadImage 下载图片（限制大小）
func downloadImage(u string) []byte {
	if u == "" {
		return nil
	}
	resp, err := mediaHTTP.Get(u)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	ct := resp.Header.Get("Content-Type")
	if ct != "" && !strings.Contains(ct, "image") {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil
	}
	return b
}

// ---------- iTunes Search（兜底 + 封面） ----------

// itunesResp iTunes 搜索响应
type itunesResp struct {
	ResultCount int           `json:"resultCount"`
	Results     []itunesTrack `json:"results"`
}

// itunesTrack iTunes 单条结果
type itunesTrack struct {
	TrackName        string `json:"trackName"`
	ArtistName       string `json:"artistName"`
	CollectionName   string `json:"collectionName"`
	ReleaseDate      string `json:"releaseDate"`
	TrackTimeMillis  int    `json:"trackTimeMillis"`
	TrackNumber      int    `json:"trackNumber"`
	TrackCount       int    `json:"trackCount"`
	PrimaryGenreName string `json:"primaryGenreName"`
	ArtworkUrl100    string `json:"artworkUrl100"`
}

// itunesSearch 搜索 iTunes 曲库
func itunesSearch(title, artist string, limit int) (itunesResp, error) {
	var out itunesResp
	if limit <= 0 || limit > 20 {
		limit = 5
	}
	params := url.Values{}
	params.Set("term", strings.TrimSpace(title+" "+artist))
	params.Set("media", "music")
	params.Set("entity", "song")
	params.Set("limit", strconv.Itoa(limit))
	req, err := http.NewRequest("GET", "https://itunes.apple.com/search?"+params.Encode(), nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("User-Agent", mediaUserAgent)
	resp, err := mediaHTTP.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return out, fmt.Errorf("iTunes HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return out, err
	}
	// iTunes 返回的 Content-Type 有时是 text/javascript，严格 JSON 解析器会拒
	err = json.Unmarshal(b, &out)
	return out, err
}

// itunesMetaFromResult 转成 MediaMeta
func itunesMetaFromResult(r itunesTrack) MediaMeta {
	var m MediaMeta
	m.Title = r.TrackName
	m.Artist = r.ArtistName
	m.Album = r.CollectionName
	m.Year = parseYear(r.ReleaseDate)
	m.Track = r.TrackNumber
	m.TrackTotal = r.TrackCount
	m.Genre = r.PrimaryGenreName
	m.Duration = float64(r.TrackTimeMillis) / 1000.0
	return m
}

// itunesCoverURL 取大尺寸封面 URL（替换 100x100 为 600x600）
func itunesCoverURL(u string) string {
	if u == "" {
		return ""
	}
	return strings.Replace(u, "100x100", "600x600", 1)
}

// ---------- 歌词 ----------

// fetchLyrics 从 lyrics.ovh 取歌词（免密钥）
func fetchLyrics(artist, title string) string {
	artist = strings.TrimSpace(artist)
	title = strings.TrimSpace(title)
	if artist == "" || title == "" {
		return ""
	}
	u := "https://api.lyrics.ovh/v1/" + url.PathEscape(artist) + "/" + url.PathEscape(title)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", mediaUserAgent)
	resp, err := mediaHTTP.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return ""
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 512<<10))
	if err != nil {
		return ""
	}
	text := strings.TrimSpace(string(b))
	if text == "" {
		return ""
	}
	// lyrics.ovh 偶尔返回 JSON（{"lyrics":"..."}）
	if strings.HasPrefix(text, "{") {
		var obj struct {
			Lyrics string `json:"lyrics"`
		}
		if json.Unmarshal([]byte(text), &obj) == nil {
			return strings.TrimSpace(obj.Lyrics)
		}
	}
	// 去掉 "Paroles de ..." 之类的头部
	if i := strings.Index(text, "\n"); i > 0 && i < 200 {
		head := text[:i]
		if strings.Contains(strings.ToLower(head), "paroles de") ||
			strings.Contains(strings.ToLower(head), "lyrics of") {
			text = strings.TrimSpace(text[i+1:])
		}
	}
	return text
}

// ---------- 综合匹配 ----------

// onlineMatchResult 在线匹配结果
type onlineMatchResult struct {
	Meta   MediaMeta
	Cover  []byte
	Lyrics string
	Source string // musicbrainz / itunes
	OK     bool
	Err    string
}

// matchMusicOnline 在线匹配歌曲元数据
// 顺序：MusicBrainz（权威）→ iTunes（兜底 + 封面）
func matchMusicOnline(title, artist, album string) onlineMatchResult {
	var res onlineMatchResult
	if title == "" {
		res.Err = "缺少歌名，无法在线匹配"
		return res
	}

	// 1) MusicBrainz
	if cands, err := mbSearchRecording(title, artist, 5); err == nil && len(cands) > 0 {
		best := pickBestRecording(cands, title, artist)
		m := mbMetaFromRecording(best)
		// 专辑名冲突时以在线为准
		if m.Title != "" {
			res.Meta = m
			res.Source = "musicbrainz"
			res.OK = true
			// 封面
			mbid, _ := mbLookupRelease(m.Album, m.Artist)
			if mbid != "" {
				if c := mbFetchCover(mbid); c != nil {
					res.Cover = c
				}
			}
		}
	}

	// 2) iTunes 兜底（补封面/时长/专辑，或 MB 无结果时整体使用）
	if it, err := itunesSearch(title, artist, 5); err == nil && it.ResultCount > 0 {
		r := it.Results[0]
		m := itunesMetaFromResult(r)
		if !res.OK {
			res.Meta = m
			res.Source = "itunes"
			res.OK = true
		} else {
			// 补充缺失项
			if res.Meta.Album == "" {
				res.Meta.Album = m.Album
			}
			if res.Meta.Year == 0 {
				res.Meta.Year = m.Year
			}
			if res.Meta.Duration <= 0 {
				res.Meta.Duration = m.Duration
			}
			if res.Meta.Track == 0 {
				res.Meta.Track = m.Track
			}
			if res.Meta.TrackTotal == 0 {
				res.Meta.TrackTotal = m.TrackTotal
			}
			if res.Meta.Genre == "" {
				res.Meta.Genre = m.Genre
			}
		}
		if res.Cover == nil && r.ArtworkUrl100 != "" {
			res.Cover = downloadImage(itunesCoverURL(r.ArtworkUrl100))
		}
	}

	if !res.OK {
		res.Err = "在线元数据库未找到匹配"
	}
	return res
}

// pickBestRecording 从候选中挑最匹配的
func pickBestRecording(cands []mbRecording, title, artist string) mbRecording {
	best := cands[0]
	bestScore := -1.0
	for _, c := range cands {
		score := similarity(c.Title, title)
		if artist != "" && len(c.ArtistCredit) > 0 {
			score = 0.7*score + 0.3*similarity(c.ArtistCredit[0].Artist.Name, artist)
		}
		if score > bestScore {
			bestScore = score
			best = c
		}
	}
	return best
}

// fetchLyricsFor 匹配到歌手/歌名后取歌词（带重试：先按原样，再按修正后的组合）
func fetchLyricsFor(artist, title string) string {
	if l := fetchLyrics(artist, title); l != "" {
		return l
	}
	// 去掉 feat. 部分再试
	if i := strings.Index(strings.ToLower(artist), "feat"); i > 0 {
		short := strings.TrimSpace(artist[:i])
		if l := fetchLyrics(short, title); l != "" {
			return l
		}
	}
	return ""
}
