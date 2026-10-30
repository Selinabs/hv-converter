//go:build portable

package main

import (
	"bytes"
	"compress/gzip"
	"embed"
	"io"
	"os"
	"path/filepath"
)

//go:embed payload/ffmpeg.exe.gz
var portableFS embed.FS

// portableFfmpeg 首次运行时把内嵌的 ffmpeg 解压到用户缓存目录，之后复用
func portableFfmpeg() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		return ""
	}
	cacheDir := filepath.Join(base, "HVConverter", "cache")
	target := filepath.Join(cacheDir, "ffmpeg-"+appVersion+".exe")
	if st, err := os.Stat(target); err == nil && st.Size() > 1024*1024 {
		return target
	}
	raw, err := portableFS.ReadFile("payload/ffmpeg.exe.gz")
	if err != nil {
		return ""
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return ""
	}
	defer zr.Close()
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return ""
	}
	tmp := target + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return ""
	}
	_, err = io.Copy(f, zr)
	f.Close()
	if err != nil {
		_ = os.Remove(tmp)
		return ""
	}
	_ = os.Remove(target)
	if err := os.Rename(tmp, target); err != nil {
		return ""
	}
	return target
}
