package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"github.com/anacrolix/torrent/metainfo"
	"time"
)

type TaskMode string

const (
	ModeHTTP TaskMode = "HTTP"
	ModeHLS  TaskMode = "HLS"
	ModeP2P  TaskMode = "P2P"
	ModeWeb  TaskMode = "网页"
)

// Task 一个下载任务
type Task struct {
	ID      int
	URL     string
	Name    string
	Dir     string
	Mode    TaskMode
	Status  string
	Total   int64
	Done    int64
	Speed   float64
	Err     string
	Convert bool
	OutPath string
	Peers   []string

	ctx      context.Context
	cancel   context.CancelFunc
	lastTick time.Time
	lastDone int64
	mu       sync.Mutex
}

func (t *Task) snap() (string, string, string, int64, int64, float64, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.Name, string(t.Mode), t.Status, t.Done, t.Total, t.Speed, t.Err
}

func (t *Task) setStatus(s string) {
	t.mu.Lock()
	t.Status = s
	t.mu.Unlock()
}

func (t *Task) setErr(err error) {
	t.mu.Lock()
	if err != nil {
		t.Err = err.Error()
		t.Status = "失败"
	}
	t.mu.Unlock()
}

// addDone 更新已下载字节并计算速度
func (t *Task) addDone(n int64) {
	t.mu.Lock()
	t.Done += n
	now := time.Now()
	if t.lastTick.IsZero() {
		t.lastTick = now
		t.lastDone = t.Done
	} else if el := now.Sub(t.lastTick); el >= time.Second {
		t.Speed = float64(t.Done-t.lastDone) / el.Seconds()
		t.lastTick = now
		t.lastDone = t.Done
	}
	t.mu.Unlock()
}

func (t *Task) setTotal(n int64) {
	t.mu.Lock()
	t.Total = n
	t.mu.Unlock()
}

// ---------- 名称与协议推断 ----------

var badNameChars = regexp.MustCompile(`[\\/:*?"<>|\r\n\t]+`)

func sanitizeName(s string) string {
	s = strings.TrimSpace(s)
	s = badNameChars.ReplaceAllString(s, "_")
	s = strings.Trim(s, " .")
	if s == "" {
		s = "download"
	}
	if len([]rune(s)) > 120 {
		r := []rune(s)
		s = string(r[:120])
	}
	return s
}

func detectMode(raw string) TaskMode {
	l := strings.ToLower(strings.TrimSpace(raw))
	if isDirectMedia(l) {
		switch {
		case strings.HasPrefix(l, "magnet:") || strings.HasSuffix(l, ".torrent"):
			return ModeP2P
		case strings.Contains(l, ".m3u8"):
			return ModeHLS
		default:
			return ModeHTTP
		}
	}
	return ModeWeb
}

// guessName 从 URL 推断资源名（不含扩展名）
func guessName(raw string, mode TaskMode) string {
	if mode == ModeWeb {
		return pageHost(raw)
	}
	if mode == ModeP2P && strings.HasPrefix(strings.ToLower(raw), "magnet:") {
		if u, err := url.Parse(raw); err == nil {
			if dn := u.Query().Get("dn"); dn != "" {
				return sanitizeName(strings.TrimSuffix(filepath.Base(dn), filepath.Ext(dn)))
			}
		}
		return "torrent"
	}
	if mode == ModeP2P && strings.HasSuffix(strings.ToLower(raw), ".torrent") {
		if st, err := os.Stat(raw); err == nil && st.Mode().IsRegular() {
			if mi, err := metainfo.LoadFromFile(raw); err == nil {
				if info, err := mi.UnmarshalInfo(); err == nil && info.Name != "" {
					return sanitizeName(strings.TrimSuffix(filepath.Base(info.Name), filepath.Ext(info.Name)))
				}
			}
			return sanitizeName(strings.TrimSuffix(filepath.Base(raw), filepath.Ext(raw)))
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "download"
	}
	base := filepath.Base(u.Path)
	if base == "." || base == "/" || base == "" {
		base = u.Host
	}
	base = strings.TrimSuffix(base, filepath.Ext(base))

	// index.m3u8 / playlist.m3u8 这类通用名：改用上一级目录名
	generic := map[string]bool{
		"index": true, "playlist": true, "master": true, "main": true,
		"prog_index": true, "chunklist": true, "video": true, "hls": true,
		"manifest": true, "stream": true, "media": true, "out": true,
	}
	if generic[strings.ToLower(base)] {
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		for i := len(parts) - 2; i >= 0; i-- {
			cand := strings.TrimSuffix(parts[i], filepath.Ext(parts[i]))
			if cand != "" && !generic[strings.ToLower(cand)] {
				base = cand
				break
			}
		}
	}
	if base == "" || generic[strings.ToLower(base)] {
		base = u.Host
	}
	return sanitizeName(base)
}

// ---------- HTTP 探测 ----------

type probeResult struct {
	size    int64
	ranges  bool
	name    string
	ctype   string
}

func probeURL(client *http.Client, raw string) (*probeResult, error) {
	req, err := http.NewRequest("GET", raw, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", "bytes=0-0")
	req.Header.Set("User-Agent", "Mozilla/5.0 HVConverter/"+appVersion)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))

	pr := &probeResult{ranges: resp.StatusCode == http.StatusPartialContent}
	pr.ctype = resp.Header.Get("Content-Type")

	if resp.StatusCode == http.StatusPartialContent {
		if cr := resp.Header.Get("Content-Range"); cr != "" {
			if i := strings.LastIndex(cr, "/"); i >= 0 {
				if n, err := strconv.ParseInt(cr[i+1:], 10, 64); err == nil {
					pr.size = n
				}
			}
		}
	} else if resp.StatusCode == http.StatusOK {
		pr.size = resp.ContentLength
	} else if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	// 文件名：优先 Content-Disposition
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if name := filenameFromDisposition(cd); name != "" {
			pr.name = name
		}
	}
	return pr, nil
}

func filenameFromDisposition(cd string) string {
	lower := strings.ToLower(cd)
	if i := strings.Index(lower, "filename*="); i >= 0 {
		v := cd[i+len("filename*="):]
		if j := strings.Index(v, ";"); j >= 0 {
			v = v[:j]
		}
		v = strings.Trim(v, "\"' ")
		if k := strings.LastIndex(v, "''"); k >= 0 {
			v = v[k+2:]
		}
		if dec, err := url.QueryUnescape(v); err == nil {
			return dec
		}
	}
	if i := strings.Index(lower, "filename="); i >= 0 {
		v := cd[i+len("filename="):]
		if j := strings.Index(v, ";"); j >= 0 {
			v = v[:j]
		}
		return strings.Trim(v, "\"' ")
	}
	return ""
}

// downloadHTTP 多连接分段下载
func downloadHTTP(ctx context.Context, t *Task, client *http.Client, log func(string)) (string, error) {
	pr, err := probeURL(client, t.URL)
	if err != nil {
		log("探测失败，回退单线程：" + err.Error())
		pr = &probeResult{}
	}
	if pr.size > 0 {
		t.setTotal(pr.size)
	}

	ext := ""
	name := t.Name
	if pr.name != "" {
		name = sanitizeName(strings.TrimSuffix(pr.name, filepath.Ext(pr.name)))
		ext = filepath.Ext(pr.name)
	}
	if ext == "" {
		if u, err := url.Parse(t.URL); err == nil {
			ext = filepath.Ext(u.Path)
		}
	}
	if ext == "" {
		ext = ".bin"
	}
	out := filepath.Join(t.Dir, name+ext)

	f, err := os.Create(out)
	if err != nil {
		return "", err
	}
	defer f.Close()

	const parts = 8
	const minSeg = 4 << 20 // 4MB 以下不分段
	if pr.ranges && pr.size > minSeg {
		chunk := pr.size / parts
		var wg sync.WaitGroup
		sem := make(chan struct{}, parts)
		errCh := make(chan error, parts)
		for i := 0; i < parts; i++ {
			start := int64(i) * chunk
			end := start + chunk - 1
			if i == parts-1 {
				end = pr.size - 1
			}
			wg.Add(1)
			go func(idx int, s, e int64) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				if err := fetchRange(ctx, client, t, f, s, e); err != nil {
					errCh <- err
				}
			}(i, start, end)
		}
		wg.Wait()
		close(errCh)
		if err := <-errCh; err != nil {
			return "", err
		}
		return out, nil
	}

	// 单线程
	req, err := http.NewRequestWithContext(ctx, "GET", t.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 HVConverter/"+appVersion)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	buf := make([]byte, 128<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.WriteAt(buf[:n], t.Done); werr != nil {
				return "", werr
			}
			t.addDone(int64(n))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", rerr
		}
	}
	return out, nil
}

func fetchRange(ctx context.Context, client *http.Client, t *Task, f *os.File, start, end int64) error {
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		req, err := http.NewRequestWithContext(ctx, "GET", t.URL, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
		req.Header.Set("User-Agent", "Mozilla/5.0 HVConverter/"+appVersion)
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}
		if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}
		buf := make([]byte, 128<<10)
		off := start
		ok := true
		for off <= end {
			want := int64(len(buf))
			if remain := end - off + 1; remain < want {
				want = remain
			}
			n, rerr := resp.Body.Read(buf[:want])
			if n > 0 {
				if _, werr := f.WriteAt(buf[:n], off); werr != nil {
					resp.Body.Close()
					return werr
				}
				off += int64(n)
				t.addDone(int64(n))
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				ok = false
				lastErr = rerr
				break
			}
		}
		resp.Body.Close()
		if ok && off > end {
			return nil
		}
		time.Sleep(time.Duration(attempt+1) * time.Second)
	}
	return lastErr
}

// ---------- HLS ----------

var errHLSEncrypted = errors.New("HLS 加密流")

func httpText(ctx context.Context, client *http.Client, raw string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", raw, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 HVConverter/"+appVersion)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func resolveURL(base, ref string) string {
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}

type hlsVariant struct {
	uri  string
	bw   int
}

func parseMaster(text string) []hlsVariant {
	var out []hlsVariant
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "#EXT-X-STREAM-INF") {
			continue
		}
		bw := 0
		if j := strings.Index(line, "BANDWIDTH="); j >= 0 {
			v := line[j+len("BANDWIDTH="):]
			if k := strings.IndexAny(v, ",;"); k >= 0 {
				v = v[:k]
			}
			bw, _ = strconv.Atoi(strings.TrimSpace(v))
		}
		for j := i + 1; j < len(lines); j++ {
			u := strings.TrimSpace(lines[j])
			if u == "" || strings.HasPrefix(u, "#") {
				continue
			}
			out = append(out, hlsVariant{uri: u, bw: bw})
			i = j
			break
		}
	}
	return out
}

func parseMedia(text string) (segs []string, encrypted bool) {
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-KEY") {
			encrypted = true
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		segs = append(segs, line)
	}
	return
}

// downloadHLS 并发下载分片并拼接
func downloadHLS(ctx context.Context, t *Task, client *http.Client, log func(string)) (string, error) {
	base := t.URL
	text, err := httpText(ctx, client, base)
	if err != nil {
		return "", err
	}
	if variants := parseMaster(text); len(variants) > 0 {
		sort.Slice(variants, func(i, j int) bool { return variants[i].bw > variants[j].bw })
		best := resolveURL(base, variants[0].uri)
		log(fmt.Sprintf("主列表：选最高码率 %d，子列表 %s", variants[0].bw, best))
		base = best
		text, err = httpText(ctx, client, base)
		if err != nil {
			return "", err
		}
	}

	segs, encrypted := parseMedia(text)
	if len(segs) == 0 {
		return "", errors.New("m3u8 中没有分片")
	}
	if encrypted {
		return "", errHLSEncrypted
	}

	tmp := filepath.Join(t.Dir, ".hvparts")
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)

	const workers = 8
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	errs := make(chan error, len(segs))

	for i, seg := range segs {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(idx int, ref string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			u := resolveURL(base, ref)
			dst := filepath.Join(tmp, fmt.Sprintf("%05d.ts", idx))
			if err := fetchToFile(ctx, client, t, u, dst); err != nil {
				errs <- fmt.Errorf("分片 %d: %w", idx, err)
			}
		}(i, seg)
	}
	wg.Wait()
	close(errs)
	if err := <-errs; err != nil {
		return "", err
	}

	out := filepath.Join(t.Dir, t.Name+".ts")
	f, err := os.Create(out)
	if err != nil {
		return "", err
	}
	defer f.Close()
	for i := range segs {
		p := filepath.Join(tmp, fmt.Sprintf("%05d.ts", i))
		sf, err := os.Open(p)
		if err != nil {
			return "", err
		}
		_, err = io.Copy(f, sf)
		sf.Close()
		if err != nil {
			return "", err
		}
	}
	return out, nil
}

func fetchToFile(ctx context.Context, client *http.Client, t *Task, raw, dst string) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, "GET", raw, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 HVConverter/"+appVersion)
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}
		if resp.StatusCode >= 400 {
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			time.Sleep(time.Duration(attempt+1) * time.Second)
			continue
		}
		f, err := os.Create(dst)
		if err != nil {
			resp.Body.Close()
			return err
		}
		n, err := io.Copy(f, resp.Body)
		f.Close()
		resp.Body.Close()
		if err == nil {
			t.addDone(n)
			return nil
		}
		lastErr = err
	}
	return lastErr
}

var _ = atomic.AddInt64
