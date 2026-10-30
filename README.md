# HV 视频转换器

把 HLS 分片（`.m3u8` + `.ts`）批量合并/转换为 MP4 的 Windows 小工具。

- 源目录 / 输出目录选择（输出目录留空默认 `文档\HV`）
- **输出文件名 = 源文件所在目录名**（同目录多个索引时追加索引名）
- 已存在同名 MP4 默认跳过，可勾选“覆盖已存在的 MP4”强制重转
- **批量队列 + 多线程并发**（1–4 个 ffmpeg 任务并行）
- 实时转换日志 + 每次运行生成 `task-yyyyMMdd-HHmmss.log`
- 启动时检查 GitHub Release，有新版本提示下载
- 自动探测 ffmpeg，并校验版本号（防被恶意替换成假壳）
- 三级降级策略：封装 → 只转音频 → 完整重编码

## 下载

- `HVConverter-Setup.exe`：安装版（含 ffmpeg，约 70 MB，用户级安装、可卸载）
- `HVConverter-Portable.exe`：单文件免安装版（含 ffmpeg，首次运行解压到 `%LOCALAPPDATA%\HVConverter\cache`）

## 使用

1. 双击打开，选择包含 `m3u8` 的源目录；
2. 可选：选择输出目录（默认 `文档\HV`）；
3. 选择并发数，点“开始转换”；
4. 完成后到输出目录取 MP4，日志同目录 `task-*.log`。

命令行模式（便于脚本调用）：

```powershell
HVConverter-Portable.exe -cli -src "D:\videos" -out "D:\out" -workers 3
```

## 从源码构建

需要 Go 1.21+、[rsrc](https://github.com/akavel/rsrc)、FFmpeg 可执行文件。

```powershell
# 1) 准备 ffmpeg（放到 C:\Users\<you>\Tools\ffmpeg\bin\ffmpeg.exe，或改脚本里的 $ffmpegSrc）
# 2) 编译
.\build.ps1
```

产物：`dist\hvconverter.exe`、`dist\HVConverter-Portable.exe`、`dist\HVConverter-Setup.exe`

## 目录结构

```
hvconverter/      主程序（walk GUI）
hvsetup/          安装器（含清理助手 cleanup/）
build.ps1         一键构建
```

## 说明

- 仅用于转换你有权保存的内容；DRM 保护的流媒体无法处理。
- 转换前建议保留原 `.m3u8` 与分片，方便失败重试。
