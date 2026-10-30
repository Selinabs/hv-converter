package main

import (
	"bytes"
	"compress/gzip"
	"embed"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
	"golang.org/x/sys/windows/registry"
)

//go:embed payload
var payload embed.FS

const (
	appName    = "HV 视频转换器"
	appExeName = "hvconverter.exe"
	appVersion = "1.0.0"
	regKeyPath = `Software\Microsoft\Windows\CurrentVersion\Uninstall\HVConverter`
)

var (
	user32           = syscall.NewLazyDLL("user32.dll")
	procMessageBoxW  = user32.NewProc("MessageBoxW")
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procMoveFileExW  = kernel32.NewProc("MoveFileExW")
	mbOKCancel       = 0x00000001
	mbYesNo          = 0x00000004
	mbIconInfo       = 0x00000040
	mbIconQuestion   = 0x00000020
	mbIconError      = 0x00000010
	idYes            = 6
)

const moveFileDelayUntilReboot = 0x4

// scheduleDeleteOnReboot 让系统在下次重启时删除该文件（自卸载兜底）
func scheduleDeleteOnReboot(path string) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return
	}
	_, _, _ = procMoveFileExW.Call(uintptr(unsafe.Pointer(p)), 0, uintptr(moveFileDelayUntilReboot))
}

func msgBox(text, caption string, flags int) int {
	t, _ := syscall.UTF16PtrFromString(text)
	c, _ := syscall.UTF16PtrFromString(caption)
	r, _, _ := procMessageBoxW.Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)), uintptr(uint(flags)))
	return int(r)
}

func installDir(override string) string {
	if override != "" {
		return override
	}
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = filepath.Dir(os.Getenv("USERPROFILE"))
	}
	return filepath.Join(base, "Programs", "HVConverter")
}

// extractGz 把内嵌的 .gz 解压到目标路径
func extractGz(name, dst string) error {
	raw, err := payload.ReadFile("payload/" + name)
	if err != nil {
		return err
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return err
	}
	defer zr.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, zr); err != nil {
		return err
	}
	return nil
}

func makeShortcut(lnk, target, workdir, icon string) error {
	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		// 已初始化也继续
	}
	defer ole.CoUninitialize()

	unknown, err := oleutil.CreateObject("WScript.Shell")
	if err != nil {
		return err
	}
	defer unknown.Release()
	shell, err := unknown.QueryInterface(ole.IID_IDispatch)
	if err != nil {
		return err
	}
	defer shell.Release()

	cs, err := oleutil.CallMethod(shell, "CreateShortcut", lnk)
	if err != nil {
		return err
	}
	disp := cs.ToIDispatch()
	defer disp.Release()

	_, _ = oleutil.PutProperty(disp, "TargetPath", target)
	_, _ = oleutil.PutProperty(disp, "WorkingDirectory", workdir)
	_, _ = oleutil.PutProperty(disp, "IconLocation", icon+",0")
	_, _ = oleutil.PutProperty(disp, "Description", appName)
	if _, err := oleutil.CallMethod(disp, "Save"); err != nil {
		return err
	}
	return nil
}

func writeUninstallEntry(dir string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, regKeyPath, registry.WRITE)
	if err != nil {
		return err
	}
	defer k.Close()
	exe := filepath.Join(dir, appExeName)
	uninst := filepath.Join(dir, "uninstall.exe")
	k.SetStringValue("DisplayName", appName)
	k.SetStringValue("DisplayVersion", appVersion)
	k.SetStringValue("Publisher", "HV")
	k.SetStringValue("InstallLocation", dir)
	k.SetStringValue("DisplayIcon", exe)
	k.SetStringValue("UninstallString", fmt.Sprintf(`"%s" /uninstall`, uninst))
	_ = k.SetDWordValue("NoModify", 1)
	_ = k.SetDWordValue("NoRepair", 1)
	return nil
}

func doInstall(dir string, silent bool) error {
	if !silent {
		if r := msgBox(fmt.Sprintf("将安装 %s %s 到：\r\n\r\n%s\r\n\r\n（包含 ffmpeg，约 160 MB）\r\n\r\n继续安装吗？", appName, appVersion, dir), "安装 " + appName, mbOKCancel|mbIconQuestion); r != idYes {
			return fmt.Errorf("已取消")
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("无法创建安装目录：%w", err)
	}
	if err := extractGz("app.exe.gz", filepath.Join(dir, appExeName)); err != nil {
		return fmt.Errorf("释放主程序失败：%w", err)
	}
	if err := extractGz("ffmpeg.exe.gz", filepath.Join(dir, "ffmpeg.exe")); err != nil {
		return fmt.Errorf("释放 ffmpeg 失败：%w", err)
	}
	icoPath := filepath.Join(dir, "app.ico")
	if b, err := payload.ReadFile("payload/app.ico"); err == nil {
		_ = os.WriteFile(icoPath, b, 0o644)
	}
	// 卸载程序 = 安装器自身副本
	if self, err := os.Executable(); err == nil {
		if b, err := os.ReadFile(self); err == nil {
			_ = os.WriteFile(filepath.Join(dir, "uninstall.exe"), b, 0o755)
		}
	}

	exe := filepath.Join(dir, appExeName)
	desktop := filepath.Join(os.Getenv("USERPROFILE"), "Desktop")
	startMenu := filepath.Join(os.Getenv("APPDATA"), `Microsoft\Windows\Start Menu\Programs`)
	_ = makeShortcut(filepath.Join(desktop, appName+".lnk"), exe, dir, icoPath)
	_ = os.MkdirAll(startMenu, 0o755)
	_ = makeShortcut(filepath.Join(startMenu, appName+".lnk"), exe, dir, icoPath)
	_ = writeUninstallEntry(dir)

	if !silent {
		if r := msgBox(fmt.Sprintf("安装完成！\r\n\r\n位置：%s\r\n\r\n是否立即启动？", dir), appName, mbYesNo|mbIconInfo); r == idYes {
			if c := exec.Command(exe); c != nil {
				_ = c.Start()
			}
		}
	}
	return nil
}

func doUninstall(dir string, silent bool) error {
	if !silent {
		if r := msgBox("确定要卸载 "+appName+" 吗？\r\n\r\n（已转换的视频不会删除）", appName, mbOKCancel|mbIconQuestion); r != idYes {
			return fmt.Errorf("已取消")
		}
	}
	_ = exec.Command("taskkill.exe", "/F", "/IM", appExeName).Run()

	desktop := filepath.Join(os.Getenv("USERPROFILE"), "Desktop")
	startMenu := filepath.Join(os.Getenv("APPDATA"), `Microsoft\Windows\Start Menu\Programs`)
	_ = os.Remove(filepath.Join(desktop, appName+".lnk"))
	_ = os.Remove(filepath.Join(startMenu, appName+".lnk"))

	if err := registry.DeleteKey(registry.CURRENT_USER, regKeyPath); err != nil && err != registry.ErrNotExist {
		// 忽略
	}

	// 释放独立的清理助手（几十 KB）：等本进程退出后删除整个安装目录
	cleanupExe := filepath.Join(os.Getenv("TEMP"), "hv_cleanup.exe")
	if err := extractGz("cleanup.exe.gz", cleanupExe); err == nil {
		c := exec.Command(cleanupExe, dir)
		c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
		_ = c.Start()
	}
	_ = os.Remove(filepath.Join(dir, appExeName))
	_ = os.Remove(filepath.Join(dir, "ffmpeg.exe"))
	_ = os.Remove(filepath.Join(dir, "app.ico"))

	if !silent {
		msgBox("卸载完成。", appName, mbIconInfo)
	}
	return nil
}

func main() {
	for i := 1; i < len(os.Args); i++ {
		if strings.HasPrefix(os.Args[i], "/") {
			os.Args[i] = "-" + os.Args[i][1:]
		}
	}
	dirFlag := flag.String("D", "", "安装目录")
	silent := flag.Bool("S", false, "静默安装")
	uninstall := flag.Bool("uninstall", false, "卸载")
	flag.Parse()

	dir := installDir(*dirFlag)
	if *uninstall && *dirFlag == "" {
		if self, err := os.Executable(); err == nil {
			dir = filepath.Dir(self)
		}
	}
	var err error
	if *uninstall {
		err = doUninstall(dir, *silent)
	} else {
		err = doInstall(dir, *silent)
	}
	if err != nil && !*silent {
		msgBox(err.Error(), appName, mbIconError)
	}
}
