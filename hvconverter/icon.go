package main

import (
	"bytes"
	_ "embed"
	"image"
	_ "image/png"

	"github.com/lxn/walk"
)

//go:embed appicon.png
var appIconPNG []byte

// appIcon 从内嵌 PNG 生成窗口图标（标题栏 / 任务栏）
func appIcon() *walk.Icon {
	img, _, err := image.Decode(bytes.NewReader(appIconPNG))
	if err != nil {
		return nil
	}
	ic, err := walk.NewIconFromImage(img)
	if err != nil {
		return nil
	}
	return ic
}
