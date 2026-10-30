//go:build windows

package main

import "syscall"

// sysProcAttrHidden 让 ffmpeg 子进程不弹出控制台窗口
func sysProcAttrHidden() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
}
