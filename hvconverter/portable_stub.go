//go:build !portable

package main

// portableFfmpeg 非便携版：不使用内嵌 ffmpeg
func portableFfmpeg() string { return "" }
