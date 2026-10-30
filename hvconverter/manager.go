package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Manager 下载任务管理器
type Manager struct {
	mu      sync.Mutex
	tasks   []*Task
	root    string
	workers int
	sem     chan struct{}
	log     func(string)
	p2p     *p2pEngine
	httpc   *http.Client
	seq     int
	Peers   []string
}

func NewManager(log func(string)) *Manager {
	m := &Manager{
		workers: 3,
		sem:     make(chan struct{}, 3),
		log:     log,
		httpc: &http.Client{
			Timeout: 0,
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 16,
				ForceAttemptHTTP2:   true,
				Proxy:               http.ProxyFromEnvironment,
			},
		},
	}
	return m
}

func (m *Manager) ensureP2P() *p2pEngine {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.p2p == nil {
		m.p2p = newP2PEngine(m.httpc, m.log)
	}
	return m.p2p
}

func (m *Manager) SetRoot(dir string) {
	m.mu.Lock()
	m.root = dir
	m.mu.Unlock()
}

func (m *Manager) Root() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.root
}

func (m *Manager) Tasks() []*Task {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Task, len(m.tasks))
	copy(out, m.tasks)
	return out
}

// NewTask 创建任务：资源同名目录 = 用户指定目录 / 资源名
func (m *Manager) NewTask(raw string, convert bool) (*Task, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("地址为空")
	}
	root := m.Root()
	if root == "" {
		return nil, errors.New("请先选择保存目录")
	}
	mode := detectMode(raw)
	name := guessName(raw, mode)
	dir := ""
	if mode != ModeWeb {
		dir = filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}

	m.mu.Lock()
	m.seq++
	id := m.seq
	m.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	t := &Task{
		ID: id, URL: raw, Name: name, Dir: dir, Mode: mode,
		Status: "排队中", Convert: convert, ctx: ctx, cancel: cancel,
		Peers: append([]string{}, m.Peers...),
	}
	m.mu.Lock()
	m.tasks = append(m.tasks, t)
	m.mu.Unlock()

	go m.run(t)
	return t, nil
}

func (m *Manager) Cancel(t *Task) {
	t.mu.Lock()
	cancel := t.cancel
	t.mu.Unlock()
	if cancel != nil {
		cancel()
		t.setStatus("已取消")
	}
}

func (m *Manager) run(t *Task) {
	m.sem <- struct{}{}
	defer func() { <-m.sem }()

	if t.Mode == ModeWeb {
		t.setStatus("解析网页中")
		m.log(fmt.Sprintf("[%s] 解析网页：%s", t.Name, t.URL))
		media, title, rerr := resolveVideo(t.URL, m.httpc, func(s string) { m.log("  " + s) })
		if rerr != nil {
			t.setErr(rerr)
			m.log(fmt.Sprintf("[%s] 解析失败：%v", t.Name, rerr))
			return
		}
		t.mu.Lock()
		t.URL = media
		if title != "" {
			t.Name = sanitizeName(title)
		}
		t.Mode = detectMode(media)
		t.Dir = filepath.Join(m.Root(), t.Name)
		t.mu.Unlock()
		if err := os.MkdirAll(t.Dir, 0o755); err != nil {
			t.setErr(err)
			return
		}
		m.log(fmt.Sprintf("[%s] 解析到 %s：%s", t.Name, t.Mode, media))
	}

	if t.ctx.Err() != nil {
		return
	}
	t.setStatus("下载中")
	m.log(fmt.Sprintf("[%s] 开始下载（%s）：%s", t.Name, t.Mode, t.URL))

	var files []string
	var err error
	switch t.Mode {
	case ModeHLS:
		files, err = m.downloadHLS(t)
	case ModeP2P:
		files, err = m.ensureP2P().Download(t.ctx, t.URL, t.Dir, t, m.log)
	default:
		var p string
		p, err = downloadHTTP(t.ctx, t, m.httpc, m.log)
		if p != "" {
			files = []string{p}
		}
	}
	if err != nil {
		if t.ctx.Err() != nil {
			t.setStatus("已取消")
			return
		}
		t.setErr(err)
		m.log(fmt.Sprintf("[%s] 下载失败：%v", t.Name, err))
		return
	}

	t.setStatus("已完成")
	m.log(fmt.Sprintf("[%s] 下载完成 -> %s", t.Name, t.Dir))

	if t.Convert {
		m.convertAfterDownload(t, files)
	}
}

func (m *Manager) downloadHLS(t *Task) ([]string, error) {
	p, err := downloadHLS(t.ctx, t, m.httpc, m.log)
	if err == nil {
		return []string{p}, nil
	}
	if !errors.Is(err, errHLSEncrypted) {
		return nil, err
	}
	// 加密 HLS：交给 ffmpeg 直接拉流
	m.log("[HLS] 检测到加密流，改用 ffmpeg 下载")
	ff, ferr := findFfmpeg()
	if ferr != nil {
		return nil, ferr
	}
	out := filepath.Join(t.Dir, t.Name+".ts")
	cmd := exec.CommandContext(t.ctx, ff, "-hide_banner", "-nostats", "-loglevel", "error", "-y",
		"-allowed_extensions", "ALL",
		"-protocol_whitelist", "file,http,https,tcp,tls,crypto",
		"-i", t.URL, "-c", "copy", out)
	cmd.SysProcAttr = sysProcAttrHidden()
	if b, e := cmd.CombinedOutput(); e != nil {
		return nil, fmt.Errorf("ffmpeg 拉流失败: %v %s", e, strings.TrimSpace(string(b)))
	}
	return []string{out}, nil
}

// convertAfterDownload 下载后可选转换为 mp4
func (m *Manager) convertAfterDownload(t *Task, files []string) {
	if len(files) == 0 {
		return
	}
	src := files[0]
	for _, f := range files {
		if isVideoFile(f) {
			src = f
			break
		}
	}
	if strings.EqualFold(filepath.Ext(src), ".mp4") {
		t.mu.Lock()
		t.OutPath = src
		t.mu.Unlock()
		m.log(fmt.Sprintf("[%s] 源文件已是 MP4，跳过转换", t.Name))
		return
	}
	ff, err := findFfmpeg()
	if err != nil {
		m.log("[转换] " + err.Error())
		return
	}
	out := filepath.Join(t.Dir, t.Name+".mp4")
	t.setStatus("转换中")
	m.log(fmt.Sprintf("[%s] 开始转换 -> %s", t.Name, out))

	strategies := [][]string{
		{"-c", "copy"},
		{"-c:v", "copy", "-c:a", "aac", "-b:a", "192k"},
		{"-c:v", "libx264", "-crf", "20", "-preset", "veryfast", "-c:a", "aac", "-b:a", "192k"},
	}
	for _, args := range strategies {
		full := append([]string{"-hide_banner", "-nostats", "-loglevel", "error", "-y", "-i", src, "-movflags", "+faststart"}, args...)
		full = append(full, out)
		cmd := exec.Command(ff, full...)
		cmd.SysProcAttr = sysProcAttrHidden()
		if b, e := cmd.CombinedOutput(); e == nil {
			if st, se := os.Stat(out); se == nil && st.Size() > 0 {
				t.mu.Lock()
				t.OutPath = out
				t.mu.Unlock()
				t.setStatus("已完成")
				m.log(fmt.Sprintf("[%s] 转换完成 -> %s（%.1f MB）", t.Name, out, float64(st.Size())/1024/1024))
				return
			}
		} else {
			m.log(fmt.Sprintf("[%s] 转换尝试失败：%v %s", t.Name, e, strings.TrimSpace(string(b))))
		}
	}
	t.setStatus("已完成（转换失败）")
}

func isVideoFile(p string) bool {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".mp4", ".mkv", ".ts", ".avi", ".mov", ".flv", ".webm", ".m4v":
		return true
	}
	return false
}

var _ = time.Second
