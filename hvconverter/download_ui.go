package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
)

var (
	mgr        *Manager
	urlEdit    *walk.LineEdit
	saveLabel  *walk.Label
	convertChk *walk.CheckBox
	dlTable    *walk.TableView
	dlModel    *taskTableModel
	dlLog      *walk.TextEdit
	dlStatus   *walk.Label
	saveRoot   string
)

// ---------- 任务表模型 ----------

type taskTableModel struct {
	walk.TableModelBase
}

func (m *taskTableModel) RowCount() int {
	if mgr == nil {
		return 0
	}
	return len(mgr.Tasks())
}

func (m *taskTableModel) Value(row, col int) interface{} {
	tasks := mgr.Tasks()
	if row < 0 || row >= len(tasks) {
		return ""
	}
	name, mode, status, done, total, speed, errMsg := tasks[row].snap()
	switch col {
	case 0:
		return name
	case 1:
		return mode
	case 2:
		if total > 0 {
			return fmt.Sprintf("%.1f%%", float64(done)/float64(total)*100)
		}
		return fmt.Sprintf("%.1f MB", float64(done)/1024/1024)
	case 3:
		if speed > 0 {
			return fmt.Sprintf("%.1f MB/s", speed/1024/1024)
		}
		return "-"
	case 4:
		if errMsg != "" && status == "失败" {
			return status + "：" + errMsg
		}
		return status
	}
	return ""
}

// ---------- 设置持久化 ----------

func settingsPath() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "HVConverter", "settings.txt")
}

func loadSaveRoot() string {
	b, err := os.ReadFile(settingsPath())
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "saveRoot=") {
			p := strings.TrimSpace(strings.TrimPrefix(line, "saveRoot="))
			if p != "" && dirExists(p) {
				return p
			}
		}
	}
	return ""
}

func persistSaveRoot(p string) {
	sp := settingsPath()
	_ = os.MkdirAll(filepath.Dir(sp), 0o755)
	_ = os.WriteFile(sp, []byte("saveRoot="+p+"\n"), 0o644)
}

func execOpen(p string) error {
	return exec.Command("explorer.exe", p).Start()
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// ---------- 界面动作 ----------

func appendDlLog(s string) {
	if dlLog == nil {
		return
	}
	line := fmt.Sprintf("[%s] %s", time.Now().Format("15:04:05"), s)
	dlLog.Synchronize(func() { dlLog.AppendText(line + "\r\n") })
}

func setDlStatus(s string) {
	if dlStatus == nil {
		return
	}
	dlStatus.Synchronize(func() { dlStatus.SetText(s) })
}

func chooseSaveRoot() {
	dlg := new(walk.FileDialog)
	dlg.Title = "选择保存目录（每个任务会在其下建同名子目录）"
	if saveRoot != "" {
		dlg.InitialDirPath = saveRoot
	}
	if ok, _ := dlg.ShowBrowseFolder(mw); ok {
		saveRoot = dlg.FilePath
		mgr.SetRoot(saveRoot)
		if saveLabel != nil {
			saveLabel.SetText(saveRoot)
		}
		persistSaveRoot(saveRoot)
	}
}

func pasteAndDownload() {
	if walk.Clipboard() != nil {
		if txt, err := walk.Clipboard().Text(); err == nil {
			txt = strings.TrimSpace(txt)
			if txt != "" && urlEdit != nil {
				urlEdit.SetText(txt)
			}
		}
	}
	newDownloadTask()
}

func newDownloadTask() {
	if mgr == nil {
		return
	}
	raw := strings.TrimSpace(urlEdit.Text())
	if raw == "" {
		walk.MsgBox(mw, "提示", "请先粘贴视频播放地址（m3u8 / mp4 / magnet）", walk.MsgBoxIconInformation)
		return
	}
	if saveRoot == "" {
		chooseSaveRoot()
		if saveRoot == "" {
			return
		}
	}
	convert := convertChk != nil && convertChk.Checked()
	t, err := mgr.NewTask(raw, convert)
	if err != nil {
		walk.MsgBox(mw, "无法新建任务", err.Error(), walk.MsgBoxIconError)
		return
	}
	appendDlLog(fmt.Sprintf("新建任务 #%d：%s -> %s", t.ID, t.Name, t.Dir))
	if urlEdit != nil {
		urlEdit.SetText("")
	}
	if dlModel != nil {
		dlModel.PublishRowsReset()
	}
}

func cancelSelectedTask() {
	if dlTable == nil || mgr == nil {
		return
	}
	idx := dlTable.CurrentIndex()
	tasks := mgr.Tasks()
	if idx < 0 || idx >= len(tasks) {
		walk.MsgBox(mw, "提示", "请先在列表里选中一个任务", walk.MsgBoxIconInformation)
		return
	}
	mgr.Cancel(tasks[idx])
	appendDlLog("已取消：" + tasks[idx].Name)
	dlModel.PublishRowsReset()
}

func openSaveRoot() {
	if saveRoot == "" {
		chooseSaveRoot()
		if saveRoot == "" {
			return
		}
	}
	_ = execOpen(saveRoot)
}

func refreshP2P() {
	go func() {
		setDlStatus("正在更新 P2P 加速服务器…")
		list, err := refreshTrackers(mgr.httpc, appendDlLog)
		if err != nil {
			setDlStatus("更新失败：" + err.Error())
			return
		}
		mgr.mu.Lock()
		if mgr.p2p != nil {
			mgr.p2p.mu.Lock()
			mgr.p2p.trackers = list
			mgr.p2p.mu.Unlock()
		}
		mgr.mu.Unlock()
		setDlStatus(fmt.Sprintf("P2P 加速服务器：%d 个 tracker", len(list)))
	}()
}

// downloadPage 返回“下载”标签页
func downloadPage() TabPage {
	return TabPage{
		Title:  "下载",
		Layout: VBox{},
		Children: []Widget{
			Composite{
				Layout: HBox{},
				Children: []Widget{
					Label{Text: "地址：", MinSize: Size{Width: 46}},
					LineEdit{AssignTo: &urlEdit, MinSize: Size{Width: 460}, MaxSize: Size{Width: 560}},
					PushButton{Text: "粘贴并下载", MinSize: Size{Width: 100}, MaxSize: Size{Width: 110}, OnClicked: pasteAndDownload},
					HSpacer{},
				},
			},
			Composite{
				Layout: HBox{},
				Children: []Widget{
					Label{Text: "保存目录：", MinSize: Size{Width: 74}},
					PushButton{Text: "选择…", MinSize: Size{Width: 70}, MaxSize: Size{Width: 80}, OnClicked: chooseSaveRoot},
					Label{AssignTo: &saveLabel, Text: "（未选择，必须手动指定）"},
					HSpacer{},
				},
			},
			Composite{
				Layout: HBox{},
				Children: []Widget{
					CheckBox{AssignTo: &convertChk, Text: "下载后转为 MP4"},
					PushButton{Text: "新建任务", MinSize: Size{Width: 92}, MaxSize: Size{Width: 100}, OnClicked: newDownloadTask},
					PushButton{Text: "取消选中", MinSize: Size{Width: 92}, MaxSize: Size{Width: 100}, OnClicked: cancelSelectedTask},
					PushButton{Text: "打开保存目录", MinSize: Size{Width: 108}, MaxSize: Size{Width: 120}, OnClicked: openSaveRoot},
					PushButton{Text: "更新 P2P 服务器", MinSize: Size{Width: 122}, MaxSize: Size{Width: 134}, OnClicked: refreshP2P},
					HSpacer{},
				},
			},
			Composite{
				Layout: HBox{},
				Children: []Widget{
					Label{AssignTo: &dlStatus, Text: "就绪"},
					HSpacer{},
				},
			},
			TableView{
				AssignTo:              &dlTable,
				AlternatingRowBG:      true,
				ColumnsOrderable:      false,
				MultiSelection:        false,
				Columns: []TableViewColumn{
					{Title: "名称", Width: 200},
					{Title: "方式", Width: 52},
					{Title: "进度", Width: 70},
					{Title: "速度", Width: 88},
					{Title: "状态", Width: 150},
				},
				Model: dlModel,
				MinSize: Size{Height: 160},
			},
			GroupBox{
				Title:  "下载日志",
				Layout: VBox{},
				Children: []Widget{
					TextEdit{AssignTo: &dlLog, ReadOnly: true, VScroll: true, HScroll: true, MinSize: Size{Height: 120}},
				},
			},
		},
	}
}

func startUITicker() {
	go func() {
		tk := time.NewTicker(time.Second)
		defer tk.Stop()
		for range tk.C {
			if dlTable == nil {
				continue
			}
			dlTable.Synchronize(func() {
				if dlModel != nil {
					dlModel.PublishRowsReset()
				}
			})
		}
	}()
}
