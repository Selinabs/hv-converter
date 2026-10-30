package main

import (
	"context"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

// 公开 tracker 列表（自动更新源）
var trackerSources = []string{
	"https://raw.githubusercontent.com/ngosang/trackerslist/master/trackers_best.txt",
	"https://raw.githubusercontent.com/ngosang/trackerslist/master/trackers_all.txt",
	"https://cdn.jsdelivr.net/gh/ngosang/trackerslist@master/trackers_best.txt",
}

// 离线兜底 tracker（内置，联网后会被自动更新覆盖）
var defaultTrackers = []string{
	"udp://tracker.opentrackr.org:1337/announce",
	"udp://open.stealth.si:80/announce",
	"udp://open.demonii.com:1337/announce",
	"udp://tracker.openbittorrent.com:6969/announce",
	"udp://explodie.org:6969/announce",
	"udp://tracker.torrent.eu.org:451/announce",
	"udp://tracker.moeking.me:6969/announce",
	"udp://opentracker.i2p.rocks:6969/announce",
	"https://tracker.tamersunion.org:443/announce",
	"wss://tracker.openwebtorrent.com",
}

const trackerRefreshEvery = 24 * time.Hour

func trackerCachePath() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "HVConverter", "trackers.txt")
}

// loadCachedTrackers 读取缓存；文件太旧返回 nil
func loadCachedTrackers() []string {
	p := trackerCachePath()
	st, err := os.Stat(p)
	if err != nil {
		return nil
	}
	if time.Since(st.ModTime()) > trackerRefreshEvery {
		return nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// refreshTrackers 从公开源拉取并合并 tracker 列表
func refreshTrackers(client *http.Client, log func(string)) ([]string, error) {
	seen := map[string]bool{}
	var all []string
	for _, src := range trackerSources {
		req, err := http.NewRequest("GET", src, nil)
		if err != nil {
			continue
		}
		req.Header.Set("User-Agent", "HVConverter/"+appVersion)
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		resp.Body.Close()
		if err != nil || resp.StatusCode != 200 {
			continue
		}
		for _, line := range strings.Split(string(body), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if !strings.Contains(line, "://") {
				continue
			}
			if !seen[line] {
				seen[line] = true
				all = append(all, line)
			}
		}
	}
	if len(all) == 0 {
		return nil, errors.New("所有 tracker 源都不可用")
	}
	p := trackerCachePath()
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	var sb strings.Builder
	sb.WriteString("# HV Converter tracker list, updated " + time.Now().Format(time.RFC3339) + "\n")
	for _, t := range all {
		sb.WriteString(t + "\n")
	}
	_ = os.WriteFile(p, []byte(sb.String()), 0o644)
	if log != nil {
		log(fmt.Sprintf("P2P 加速服务器已更新：%d 个 tracker", len(all)))
	}
	return all, nil
}

// trackers 返回可用 tracker：缓存 -> 刷新 -> 内置兜底
func trackers(client *http.Client, log func(string)) []string {
	if t := loadCachedTrackers(); t != nil {
		return t
	}
	if t, err := refreshTrackers(client, log); err == nil {
		return t
	}
	if log != nil {
		log("tracker 更新失败，使用内置列表")
	}
	return defaultTrackers
}

// ---------- BitTorrent ----------

type p2pEngine struct {
	mu       sync.Mutex
	trackers []string
	log      func(string)
}

func newP2PEngine(client *http.Client, log func(string)) *p2pEngine {
	p := &p2pEngine{log: log}
	p.trackers = trackers(client, log)
	// 后台定期刷新
	go func() {
		tk := time.NewTicker(trackerRefreshEvery)
		defer tk.Stop()
		for range tk.C {
			if list, err := refreshTrackers(client, log); err == nil {
				p.mu.Lock()
				p.trackers = list
				p.mu.Unlock()
			}
		}
	}()
	return p
}

func (p *p2pEngine) getTrackers() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.trackers))
	copy(out, p.trackers)
	return out
}

// Download 下载 magnet 或 .torrent，返回本地文件路径
func (p *p2pEngine) Download(ctx context.Context, raw, dir string, t *Task, log func(string)) ([]string, error) {
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = dir
	cfg.Seed = false
	cfg.NoUpload = false
	cfg.ListenPort = 0
	cfg.DisableIPv6 = false

	client, err := torrent.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("启动 BT 客户端失败: %w", err)
	}
	defer client.Close()

	var tor *torrent.Torrent
	lower := strings.ToLower(raw)
	switch {
	case strings.HasPrefix(lower, "magnet:"):
		tor, err = client.AddMagnet(raw)
		if err != nil {
			return nil, fmt.Errorf("添加 magnet 失败: %w", err)
		}
	default:
		// http(s) .torrent 或本地文件
		var data []byte
		if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
			req, rerr := http.NewRequestWithContext(ctx, "GET", raw, nil)
			if rerr != nil {
				return nil, rerr
			}
			resp, rerr := http.DefaultClient.Do(req)
			if rerr != nil {
				return nil, rerr
			}
			data, rerr = io.ReadAll(io.LimitReader(resp.Body, 16<<20))
			resp.Body.Close()
			if rerr != nil {
				return nil, rerr
			}
		} else {
			data, err = os.ReadFile(raw)
			if err != nil {
				return nil, err
			}
		}
		mi, merr := metainfo.Load(bytes.NewReader(data))
		if merr != nil {
			return nil, fmt.Errorf("解析 .torrent 失败: %w", merr)
		}
		spec := torrent.TorrentSpecFromMetaInfo(mi)
		spec.PeerAddrs = t.Peers
		tor, _, err = client.AddTorrentSpec(spec)
		if err != nil {
			return nil, err
		}
	}

	// 等待元数据
	select {
	case <-tor.GotInfo():
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(90 * time.Second):
		return nil, errors.New("获取种子元数据超时（可能没有可用 peer）")
	}

	var tiers [][]string
	for _, u := range p.getTrackers() {
		tiers = append(tiers, []string{u})
	}
	tor.AddTrackers(tiers)
	tor.DownloadAll()
	total := tor.Length()
	if total > 0 {
		t.setTotal(total)
	}

	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			done := tor.BytesCompleted()
			if total > 0 && done > 0 {
				// 用累计值驱动速率
				t.mu.Lock()
				if t.Done < done {
					t.mu.Unlock()
					t.addDone(done - t.Done)
				} else {
					t.mu.Unlock()
				}
			}
			if total > 0 && done >= total {
				goto doneAll
			}
		}
	}

doneAll:
	var paths []string
	for _, f := range tor.Files() {
		pth := f.Path()
		if !filepath.IsAbs(pth) {
			pth = filepath.Join(dir, pth)
		}
		if st, err := os.Stat(pth); err == nil && st.Size() > 0 {
			paths = append(paths, pth)
		}
	}
	if len(paths) == 0 {
		return nil, errors.New("下载完成但没有找到文件")
	}
	return paths, nil
}
