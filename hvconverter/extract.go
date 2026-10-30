package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"time"
	"os"
	"regexp"
	"sort"
	"strings"

)

var (
	reM3U8 = regexp.MustCompile(`(?i)https?://[^\s"'<>\\]+?\.m3u8(?:\?[^\s"'<>\\]*)?`)
	reMP4  = regexp.MustCompile(`(?i)https?://[^\s"'<>\\]+?\.mp4(?:\?[^\s"'<>\\]*)?`)
	reSrc  = regexp.MustCompile(`(?i)<(?:video|source)[^>]+src\s*=\s*["']([^"']+)["']`)
	reOG   = regexp.MustCompile(`(?i)<meta[^>]+property\s*=\s*["']og:video(?::[\w:]+)?["'][^>]+content\s*=\s*["']([^"']+)["']`)
	reTitle = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	reOGT  = regexp.MustCompile(`(?i)<meta[^>]+property\s*=\s*["']og:title["'][^>]+content\s*=\s*["']([^"']+)["']`)
	reIframe = regexp.MustCompile(`(?i)<iframe[^>]+src\s*=\s*["']([^"']+)["']`)
	reJSONURL = regexp.MustCompile(`(?i)"contentUrl"\s*:\s*"([^"]+)"`)
)

func isDirectMedia(raw string) bool {
	l := strings.ToLower(raw)
	if strings.HasPrefix(l, "magnet:") {
		return true
	}
	for _, ext := range []string{".m3u8", ".mp4", ".ts", ".flv", ".mkv", ".mov", ".webm", ".m4v", ".torrent"} {
		if strings.Contains(l, ext) {
			return true
		}
	}
	return false
}

func httpGetString(ctx context.Context, client *http.Client, raw string) (string, string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", raw, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", "", err
	}
	return string(b), resp.Request.URL.String(), nil
}

const browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36 Edg/131.0.0.0"

// pickBest 从候选里挑最合适的播放地址：优先 m3u8
func pickBest(cands []string) string {
	var m3u8, mp4 []string
	seen := map[string]bool{}
	for _, c := range cands {
		c = strings.TrimSpace(strings.TrimRight(c, `\`))
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		l := strings.ToLower(c)
		switch {
		case strings.Contains(l, ".m3u8"):
			m3u8 = append(m3u8, c)
		case strings.Contains(l, ".mp4"):
			mp4 = append(mp4, c)
		}
	}
	if len(m3u8) > 0 {
		// 优先带高清/主列表标记的
		sort.SliceStable(m3u8, func(i, j int) bool {
			hi := func(s string) bool {
				l := strings.ToLower(s)
				return strings.Contains(l, "1080") || strings.Contains(l, "hd") || strings.Contains(l, "high")
			}
			return hi(m3u8[i]) && !hi(m3u8[j])
		})
		return m3u8[0]
	}
	if len(mp4) > 0 {
		return mp4[0]
	}
	return ""
}

func pageTitle(html, fallback string) string {
	if m := reOGT.FindStringSubmatch(html); len(m) > 1 {
		if t := strings.TrimSpace(m[1]); t != "" {
			return t
		}
	}
	if m := reTitle.FindStringSubmatch(html); len(m) > 1 {
		t := strings.TrimSpace(m[1])
		t = strings.ReplaceAll(t, "\n", " ")
		if t != "" {
			return t
		}
	}
	return fallback
}

// extractFromHTML 直接从 HTML 找播放地址，必要时跟进 iframe
func extractFromHTML(ctx context.Context, client *http.Client, pageURL string, depth int, log func(string)) (string, string, error) {
	html, finalURL, err := httpGetString(ctx, client, pageURL)
	if err != nil {
		return "", "", err
	}
	title := pageTitle(html, "")
	var cands []string
	for _, re := range []*regexp.Regexp{reM3U8, reMP4, reSrc, reOG, reJSONURL} {
		for _, m := range re.FindAllStringSubmatch(html, -1) {
			v := strings.TrimSpace(m[1])
			if v == "" {
				continue
			}
			cands = append(cands, resolveURL(finalURL, v))
		}
	}
	if best := pickBest(cands); best != "" {
		return best, title, nil
	}
	if depth <= 0 {
		return "", title, fmt.Errorf("页面里没有找到可直接下载的播放地址")
	}
	// 跟进 iframe
	for i, m := range reIframe.FindAllStringSubmatch(html, -1) {
		if i >= 4 {
			break
		}
		src := resolveURL(finalURL, strings.TrimSpace(m[1]))
		if log != nil {
			log("尝试解析内嵌页面：" + src)
		}
		if u, _, err := extractFromHTML(ctx, client, src, depth-1, log); err == nil && u != "" {
			return u, title, nil
		}
	}
	return "", title, fmt.Errorf("页面里没有找到可直接下载的播放地址")
}

func findEdge() string {
	for _, p := range []string{
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
		`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// extractWithBrowser 用 Edge 无头模式渲染页面并导出 DOM，再从 DOM 里找播放地址
func extractWithBrowser(pageURL string, log func(string)) (string, string, error) {
	exe := findEdge()
	if exe == "" {
		return "", "", fmt.Errorf("没有找到 Edge/Chrome，无法解析动态网页")
	}
	dir, err := os.MkdirTemp("", "hvdom")
	if err != nil {
		return "", "", err
	}
	defer os.RemoveAll(dir)

	args := []string{
		"--headless=new",
		"--disable-gpu",
		"--mute-audio",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-extensions",
		"--user-data-dir=" + dir,
		"--virtual-time-budget=12000",
		"--dump-dom",
		pageURL,
	}
	cmd := exec.Command(exe, args...)
	cmd.SysProcAttr = sysProcAttrHidden()
	out, err := cmd.Output()
	if err != nil && len(out) == 0 {
		return "", "", fmt.Errorf("无头浏览器执行失败: %v", err)
	}
	html := string(out)
	title := pageTitle(html, "")

	var cands []string
	for _, re := range []*regexp.Regexp{reM3U8, reMP4, reSrc, reOG, reJSONURL} {
		for _, m := range re.FindAllStringSubmatch(html, -1) {
			v := strings.TrimSpace(m[1])
			if v != "" {
				cands = append(cands, resolveURL(pageURL, v))
			}
		}
	}
	if best := pickBest(cands); best != "" {
		if log != nil {
			log(fmt.Sprintf("无头浏览器渲染后找到 %d 个候选地址", len(cands)))
		}
		return best, title, nil
	}
	if log != nil {
		log("渲染后的页面里仍然没有媒体地址")
	}
	return "", title, fmt.Errorf("渲染后仍未找到媒体地址（可能需要登录/会员，或地址在加密脚本里）")
}
// resolveVideo 解析网页地址，返回播放地址和标题
func resolveVideo(pageURL string, client *http.Client, log func(string)) (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	if log != nil {
		log("先尝试直接解析网页…")
	}
	if u, title, err := extractFromHTML(ctx, client, pageURL, 2, log); err == nil && u != "" {
		return u, title, nil
	} else if log != nil && err != nil {
		log("直接解析失败：" + err.Error())
	}

	if log != nil {
		log("改用无头浏览器（Edge）执行页面脚本…")
	}
	return extractWithBrowser(pageURL, log)
}

// pageHost 用于网页任务的占位名
func pageHost(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return sanitizeName(u.Host)
	}
	return "video"
}
