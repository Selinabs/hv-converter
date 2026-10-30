package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
)

const (
	appTitle   = "HV 视频转换器  ·  m3u8 → MP4"
	appVersion = "1.0.0"
	ffmpegHint = "找不到可用的 ffmpeg.exe，请安装 FFmpeg 并加入 PATH。"
)

// GitHub 仓库信息（版本检查用）
var (
	repoOwner = "Selinabs"
	repoName  = "hv-converter"
)

var (
	srcLabel    *walk.Label
	outLabel    *walk.Label
	logEdit     *walk.TextEdit
	statusLabel *walk.Label
	forceChk    *walk.CheckBox
	workerBox   *walk.ComboBox
	startBtn    *walk.PushButton
	stopBtn     *walk.PushButton
	mw          *walk.MainWindow

	srcPath string
	outPath string

	stopping int32
	running  int32
)

// ---------- 工具函数 ----------

func defaultOutDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return `C:\HV`
	}
	dir := filepath.Join(home, "Documents", "HV")
	if _, e := os.Stat(filepath.Join(home, "Documents")); e != nil {
		dir = filepath.Join(home, "HV")
	}
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

func validFfmpeg(p string) bool {
	if _, err := os.Stat(p); err != nil {
		return false
	}
	out, err := exec.Command(p, "-version").Output()
	return err == nil && strings.Contains(string(out), "ffmpeg version")
}

// findFfmpeg 返回可用的 ffmpeg 路径，并校验它能输出版本号（防止被替换成假壳）
func findFfmpeg() (string, error) {
	if p := portableFfmpeg(); p != "" && validFfmpeg(p) {
		return p, nil
	}
	candidates := []string{}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "ffmpeg.exe"))
	}
	if la := os.Getenv("LOCALAPPDATA"); la != "" {
		candidates = append(candidates,
			filepath.Join(la, `Programs\HVConverter`, "ffmpeg.exe"),
			filepath.Join(la, `HVConverter`, "ffmpeg.exe"),
		)
	}
	if p, err := exec.LookPath("ffmpeg.exe"); err == nil {
		candidates = append(candidates, p)
	}
	candidates = append(candidates,
		filepath.Join(os.Getenv("LOCALAPPDATA"), `Microsoft\WinGet\Links\ffmpeg.exe`),
		`C:\Users\chenqi\Tools\ffmpeg\bin\ffmpeg.exe`,
		`C:\Program Files\ffmpeg\bin\ffmpeg.exe`,
	)
	for _, c := range candidates {
		if validFfmpeg(c) {
			return c, nil
		}
	}
	return "", fmt.Errorf("%s", ffmpegHint)
}

type job struct {
	m3u8    string
	outFile string
	dirName string
}

// scan 递归扫描 .m3u8，输出名 = 源文件所在目录名
func scan(src, out string) ([]job, []string, error) {
	var dirs []string
	var jobs []job
	seenDirs := map[string]bool{}
	perDir := map[string]int{}

	_ = filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if strings.EqualFold(filepath.Ext(path), ".m3u8") {
			perDir[filepath.Dir(path)]++
		}
		return nil
	})

	_ = filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if !strings.EqualFold(filepath.Ext(path), ".m3u8") {
			return nil
		}
		d := filepath.Dir(path)
		if !seenDirs[d] {
			seenDirs[d] = true
			dirs = append(dirs, d)
		}
		name := filepath.Base(d)
		if name == "" || name == `\` {
			name = "video"
		}
		outName := name + ".mp4"
		if perDir[d] > 1 {
			outName = name + "_" + strings.TrimSuffix(filepath.Base(path), filepath.Ext(path)) + ".mp4"
		}
		jobs = append(jobs, job{
			m3u8:    path,
			outFile: filepath.Join(out, outName),
			dirName: name,
		})
		return nil
	})

	sort.Slice(jobs, func(i, j int) bool { return jobs[i].m3u8 < jobs[j].m3u8 })
	sort.Strings(dirs)
	return jobs, dirs, nil
}

type logger struct {
	ui  func(string)
	f   *os.File
	buf *bufio.Writer
	mu  chan struct{}
}

func newLogger(ui func(string), path string) *logger {
	l := &logger{ui: ui, mu: make(chan struct{}, 1)}
	l.mu <- struct{}{}
	if path != "" {
		if f, err := os.Create(path); err == nil {
			l.f = f
			_, _ = f.Write([]byte{0xEF, 0xBB, 0xBF}) // UTF-8 BOM
			l.buf = bufio.NewWriter(f)
		}
	}
	return l
}

func (l *logger) Log(format string, a ...interface{}) {
	msg := fmt.Sprintf(format, a...)
	line := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), msg)
	<-l.mu
	defer func() { l.mu <- struct{}{} }()
	if l.buf != nil {
		fmt.Fprintln(l.buf, line)
		l.buf.Flush()
	}
	if l.ui != nil {
		l.ui(line + "\r\n")
	}
}

func (l *logger) Close() {
	<-l.mu
	defer func() { l.mu <- struct{}{} }()
	if l.buf != nil {
		l.buf.Flush()
	}
	if l.f != nil {
		l.f.Close()
	}
}

// convert 依次尝试三种策略
func convert(ffmpeg string, j job, log *logger) error {
	common := []string{
		"-hide_banner", "-nostats", "-loglevel", "error", "-y",
		"-allowed_extensions", "ALL",
		"-protocol_whitelist", "file,http,https,tcp,tls,crypto",
		"-i", j.m3u8,
		"-movflags", "+faststart",
	}
	strategies := []struct {
		name string
		args []string
	}{
		{"封装(不重编码)", []string{"-c", "copy"}},
		{"视频复制+音频转AAC", []string{"-c:v", "copy", "-c:a", "aac", "-b:a", "192k"}},
		{"完整重编码(H.264/AAC)", []string{"-c:v", "libx264", "-crf", "20", "-preset", "veryfast", "-c:a", "aac", "-b:a", "192k"}},
	}

	for _, s := range strategies {
		if atomic.LoadInt32(&stopping) == 1 {
			return fmt.Errorf("已停止")
		}
		log.Log("  [%s] 尝试：%s", j.dirName, s.name)
		args := append(append([]string{}, common...), s.args...)
		args = append(args, j.outFile)

		cmd := exec.Command(ffmpeg, args...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		cmd.SysProcAttr = sysProcAttrHidden()
		start := time.Now()
		err := cmd.Run()
		if err == nil {
			if st, serr := os.Stat(j.outFile); serr == nil && st.Size() > 0 {
				log.Log("  [%s] 完成：%s（%.1f MB，耗时 %.1f 秒）", j.dirName,
					filepath.Base(j.outFile), float64(st.Size())/1024/1024, time.Since(start).Seconds())
				return nil
			}
			log.Log("  [%s] 输出为空，换下一种方式", j.dirName)
			continue
		}
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[len(msg)-300:]
		}
		log.Log("  [%s] 失败：%v %s", j.dirName, err, strings.ReplaceAll(msg, "\n", " | "))
	}
	return fmt.Errorf("所有方式都失败")
}

// runConversion 支持并发 worker
func runConversion(src, out string, force bool, workers int, log *logger) {
	if strings.TrimSpace(src) == "" {
		log.Log("错误：未选择源目录。")
		return
	}
	if st, err := os.Stat(src); err != nil || !st.IsDir() {
		log.Log("错误：源目录不存在：%s", src)
		return
	}
	if strings.TrimSpace(out) == "" {
		out = defaultOutDir()
	}
	_ = os.MkdirAll(out, 0o755)

	ffmpeg, err := findFfmpeg()
	if err != nil {
		log.Log("错误：%v", err)
		return
	}
	log.Log("FFmpeg：%s", ffmpeg)
	log.Log("扫描源目录：%s", src)
	log.Log("输出目录：%s", out)

	jobs, dirs, err := scan(src, out)
	if err != nil {
		log.Log("扫描失败：%v", err)
		return
	}
	log.Log("发现 %d 个目录、%d 个 .m3u8 索引，并发数 %d。", len(dirs), len(jobs), workers)
	for _, d := range dirs {
		log.Log("  扫描目录：%s", d)
	}
	if len(jobs) == 0 {
		log.Log("没有找到 .m3u8 文件。")
		return
	}
	if workers < 1 {
		workers = 1
	}
	if workers > len(jobs) {
		workers = len(jobs)
	}

	var (
		wg              sync.WaitGroup
		ok, skip, fail  int32
		done            int32
		total           = int32(len(jobs))
		ch              = make(chan job)
	)

	worker := func() {
		defer wg.Done()
		for j := range ch {
			if atomic.LoadInt32(&stopping) == 1 {
				atomic.AddInt32(&fail, 0)
				continue
			}
			log.Log("(%d/%d) %s", atomic.AddInt32(&done, 1), total, j.m3u8)
			log.Log("  输出 -> %s", j.outFile)

			if st, e := os.Stat(j.outFile); e == nil && st.Size() > 0 && !force {
				log.Log("  [%s] 跳过：已存在（%.1f MB）", j.dirName, float64(st.Size())/1024/1024)
				atomic.AddInt32(&skip, 1)
			} else if err := convert(ffmpeg, j, log); err != nil {
				log.Log("  [%s] 错误：%v", j.dirName, err)
				atomic.AddInt32(&fail, 1)
			} else {
				atomic.AddInt32(&ok, 1)
			}
			setStatus(fmt.Sprintf("进度 %d/%d ｜ 成功 %d 跳过 %d 失败 %d",
				atomic.LoadInt32(&done), total,
				atomic.LoadInt32(&ok), atomic.LoadInt32(&skip), atomic.LoadInt32(&fail)))
		}
	}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go worker()
	}
	for _, j := range jobs {
		if atomic.LoadInt32(&stopping) == 1 {
			break
		}
		ch <- j
	}
	close(ch)
	wg.Wait()

	summary := fmt.Sprintf("完成：成功 %d，跳过 %d，失败 %d", ok, skip, fail)
	log.Log(summary)
	setStatus(summary)
}

// ---------- 版本检查 ----------

type releaseInfo struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
	Name    string `json:"name"`
}

func versionGreater(latest, current string) bool {
	norm := func(s string) []int {
		s = strings.TrimPrefix(strings.TrimSpace(s), "v")
		parts := strings.Split(s, ".")
		out := []int{0, 0, 0}
		for i := 0; i < len(parts) && i < 3; i++ {
			n, _ := strconv.Atoi(strings.Split(parts[i], "-")[0])
			out[i] = n
		}
		return out
	}
	a, b := norm(latest), norm(current)
	for i := 0; i < 3; i++ {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

func checkUpdate(ui func(string)) {
	if repoOwner == "OWNER" {
		return
	}
	client := &http.Client{Timeout: 6 * time.Second}
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", repoOwner, repoName)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", "hvconverter/"+appVersion)
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return
	}
	var rel releaseInfo
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return
	}
	if !versionGreater(rel.TagName, appVersion) {
		if ui != nil {
			ui(fmt.Sprintf("当前已是最新版本 v%s", appVersion))
		}
		return
	}
	ui(fmt.Sprintf("发现新版本 %s（当前 v%s）", rel.TagName, appVersion))
	if mw != nil {
		mw.Synchronize(func() {
			if walk.MsgBox(mw, "发现新版本",
				fmt.Sprintf("发现新版本 %s（当前 v%s）。\r\n\r\n是否打开下载页面？", rel.TagName, appVersion),
				walk.MsgBoxYesNo|walk.MsgBoxIconInformation) == walk.DlgCmdYes {
				_ = exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", rel.HTMLURL).Start()
			}
		})
	}
}

// ---------- GUI ----------

func appendLog(s string) {
	if logEdit == nil {
		return
	}
	logEdit.Synchronize(func() { logEdit.AppendText(s) })
}

func setStatus(s string) {
	if statusLabel == nil {
		return
	}
	statusLabel.Synchronize(func() { statusLabel.SetText(s) })
}

func browseSrc() {
	dlg := new(walk.FileDialog)
	dlg.Title = "选择源目录（包含 m3u8 的文件夹）"
	if srcPath != "" {
		dlg.InitialDirPath = srcPath
	}
	if ok, _ := dlg.ShowBrowseFolder(mw); ok {
		srcPath = dlg.FilePath
		if srcLabel != nil {
			srcLabel.SetText(srcPath)
		}
	}
}

func browseOut() {
	dlg := new(walk.FileDialog)
	dlg.Title = "选择输出目录"
	if outPath != "" {
		dlg.InitialDirPath = outPath
	}
	if ok, _ := dlg.ShowBrowseFolder(mw); ok {
		outPath = dlg.FilePath
		if outLabel != nil {
			outLabel.SetText(outPath)
		}
	}
}

func workers() int {
	if workerBox == nil {
		return 2
	}
	n, err := strconv.Atoi(workerBox.Text())
	if err != nil || n < 1 {
		return 2
	}
	return n
}

func startConversion() {
	if atomic.LoadInt32(&running) == 1 {
		return
	}
	if srcPath == "" {
		walk.MsgBox(mw, "提示", "请先选择源目录。", walk.MsgBoxIconInformation)
		return
	}
	out := outPath
	if out == "" {
		out = defaultOutDir()
		outPath = out
		if outLabel != nil {
			outLabel.SetText(out)
		}
	}
	_ = os.MkdirAll(out, 0o755)

	logPath := filepath.Join(out, fmt.Sprintf("task-%s.log", time.Now().Format("20060102-150405")))
	log := newLogger(appendLog, logPath)
	defer log.Close()

	log.Log("==== HV 视频转换器 v%s ====", appVersion)
	log.Log("任务日志：%s", logPath)

	atomic.StoreInt32(&running, 1)
	atomic.StoreInt32(&stopping, 0)
	startBtn.SetEnabled(false)
	stopBtn.SetEnabled(true)

	runConversion(srcPath, out, forceChk.Checked(), workers(), log)

	startBtn.Synchronize(func() { startBtn.SetEnabled(true) })
	stopBtn.Synchronize(func() { stopBtn.SetEnabled(false) })
	atomic.StoreInt32(&running, 0)
}

func stopConversion() {
	atomic.StoreInt32(&stopping, 1)
	setStatus("正在停止（等待当前任务结束）…")
}

func openOutDir() {
	d := outPath
	if d == "" {
		d = defaultOutDir()
		outPath = d
		if outLabel != nil {
			outLabel.SetText(d)
		}
	}
	_ = exec.Command("explorer.exe", d).Start()
}

func main() {
	for i := 1; i < len(os.Args); i++ {
		if strings.HasPrefix(os.Args[i], "/") {
			os.Args[i] = "-" + os.Args[i][1:]
		}
	}
	cli := flag.Bool("cli", false, "命令行模式（无界面）")
	src := flag.String("src", "", "源目录")
	out := flag.String("out", "", "输出目录")
	force := flag.Bool("force", false, "覆盖已存在的 MP4")
	workersFlag := flag.Int("workers", 2, "并发转换数")
	noUpdate := flag.Bool("no-update-check", false, "跳过版本检查")
	flag.Parse()

	if *cli || *src != "" {
		logPath := ""
		if *out != "" {
			_ = os.MkdirAll(*out, 0o755)
			logPath = filepath.Join(*out, fmt.Sprintf("task-%s.log", time.Now().Format("20060102-150405")))
		}
		l := newLogger(func(s string) { fmt.Print(s) }, logPath)
		defer l.Close()
		runConversion(*src, *out, *force, *workersFlag, l)
		return
	}

	if v := os.Getenv("HV_SRC"); v != "" {
		srcPath = v
	}
	if v := os.Getenv("HV_OUT"); v != "" {
		outPath = v
	} else {
		outPath = defaultOutDir()
	}

	if err := (MainWindow{
		AssignTo: &mw,
		Title:    appTitle + "  v" + appVersion,
		Size:     Size{Width: 920, Height: 660},
		MinSize:  Size{Width: 760, Height: 500},
		Layout:   VBox{},
		Children: []Widget{
			Composite{
				Layout: VBox{},
				Children: []Widget{
					Composite{
						Layout: HBox{},
						Children: []Widget{
							PushButton{Text: "选择源目录…", MinSize: Size{Width: 118}, MaxSize: Size{Width: 140}, OnClicked: browseSrc},
							Label{AssignTo: &srcLabel, Text: "（未选择）"},
							HSpacer{},
						},
					},
					Composite{
						Layout: HBox{},
						Children: []Widget{
							PushButton{Text: "输出目录…", MinSize: Size{Width: 118}, MaxSize: Size{Width: 140}, OnClicked: browseOut},
							Label{AssignTo: &outLabel, Text: outPath},
							HSpacer{},
						},
					},
					Composite{
						Layout: HBox{},
						Children: []Widget{
							CheckBox{AssignTo: &forceChk, Text: "覆盖已存在的 MP4"},
							Label{Text: "并发数："},
							ComboBox{AssignTo: &workerBox, MinSize: Size{Width: 60}, MaxSize: Size{Width: 70},
								Model: []string{"1", "2", "3", "4"}, CurrentIndex: 1},
							PushButton{AssignTo: &startBtn, Text: "开始转换", MinSize: Size{Width: 96}, MaxSize: Size{Width: 110}, OnClicked: startConversion},
							PushButton{AssignTo: &stopBtn, Text: "停止", MinSize: Size{Width: 74}, MaxSize: Size{Width: 90}, Enabled: false, OnClicked: stopConversion},
							PushButton{Text: "打开输出目录", MinSize: Size{Width: 110}, MaxSize: Size{Width: 130}, OnClicked: openOutDir},
							HSpacer{},
						},
					},
					Composite{
						Layout: HBox{},
						Children: []Widget{
							Label{AssignTo: &statusLabel, Text: "就绪"},
							HSpacer{},
						},
					},
				},
			},
			GroupBox{
				Title:  "转换日志",
				Layout: VBox{},
				Children: []Widget{
					TextEdit{
						AssignTo: &logEdit, ReadOnly: true, VScroll: true, HScroll: true,
						Text: "选择源目录后点击“开始转换”。\r\n输出文件名 = 源文件所在目录名。重名文件默认跳过，勾选“覆盖已存在的 MP4”可强制重转。\r\n",
					},
				},
			},
		},
	}).Create(); err != nil {
		walk.MsgBox(nil, "启动失败", err.Error(), walk.MsgBoxIconError)
		fmt.Fprintln(os.Stderr, "create failed:", err)
		return
	}

	if srcPath != "" && srcLabel != nil {
		srcLabel.SetText(srcPath)
	}
	if outPath != "" && outLabel != nil {
		outLabel.SetText(outPath)
	}

	mw.Starting().Attach(func() {
		if !*noUpdate {
			go checkUpdate(func(s string) {
				appendLog("[版本检查] " + s + "\r\n")
			})
		}
	})

	mw.Run()
}
