package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// runDownloadCLI 命令行下载模式（也用于自动化测试）
func runDownloadCLI(rawURL, saveDir string, convert bool, workers int, peers string) {
	if strings.TrimSpace(rawURL) == "" {
		fmt.Println("缺少 -dl 地址")
		os.Exit(2)
	}
	if strings.TrimSpace(saveDir) == "" {
		fmt.Println("缺少 -save 保存目录")
		os.Exit(2)
	}
	if workers < 1 {
		workers = 1
	}

	m := NewManager(func(s string) { fmt.Println(s) })
	m.sem = make(chan struct{}, workers)
	m.SetRoot(saveDir)
	if peers != "" {
		m.Peers = strings.Split(peers, ",")
	}

	t, err := m.NewTask(rawURL, convert)
	if err != nil {
		fmt.Println("创建任务失败:", err)
		os.Exit(1)
	}
	fmt.Printf("任务 #%d  名称=%s\n目录=%s\n方式=%s\n", t.ID, t.Name, t.Dir, t.Mode)

	deadline := time.Now().Add(6 * time.Hour)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		_, _, status, done, total, speed, errMsg := t.snap()
		if total > 0 {
			fmt.Printf("\r进度 %.1f%%  %.2f MB/s   %s        ",
				float64(done)/float64(total)*100, speed/1024/1024, status)
		} else {
			fmt.Printf("\r已下载 %.1f MB  %.2f MB/s   %s        ", float64(done)/1024/1024, speed/1024/1024, status)
		}
		switch {
		case status == "已完成", status == "已完成（转换失败）":
			fmt.Println()
			if t.OutPath != "" {
				fmt.Println("输出:", t.OutPath)
			}
			fmt.Println("完成")
			return
		case status == "失败":
			fmt.Println()
			fmt.Println("失败:", errMsg)
			os.Exit(1)
		case status == "已取消":
			fmt.Println()
			fmt.Println("已取消")
			return
		}
	}
	fmt.Println()
	fmt.Println("超时")
}
