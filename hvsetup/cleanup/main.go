package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"
)

var kernel32 = syscall.NewLazyDLL("kernel32.dll")
var procMoveFileExW = kernel32.NewProc("MoveFileExW")

const moveFileDelayUntilReboot = 0x4

func scheduleDeleteOnReboot(path string) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return
	}
	_, _, _ = procMoveFileExW.Call(uintptr(unsafe.Pointer(p)), 0, uintptr(moveFileDelayUntilReboot))
}

func main() {
	if len(os.Args) < 2 {
		os.Exit(2)
	}
	dir := os.Args[1]
	time.Sleep(3 * time.Second)

	for _, name := range []string{
		"hvconverter.exe", "ffmpeg.exe", "app.ico",
		"uninstall.exe", "uninstall.exe.old",
	} {
		_ = os.Remove(filepath.Join(dir, name))
	}
	_ = os.Remove(dir)

	self, _ := os.Executable()
	if self != "" {
		scheduleDeleteOnReboot(self)
		c := exec.Command("cmd.exe", "/c", fmt.Sprintf(`ping 127.0.0.1 -n 2 >nul & del /f /q "%s"`, self))
		c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
		_ = c.Start()
	}
}
