# HV 视频转换器

Windows 小工具：**下载** + **转换**（HLS 分片合并）二合一，带图形界面、并发任务队列和自动更新检查。

## 下载引擎

| 协议 | 说明 |
|---|---|
| HTTP(S) 直链 | 多连接分段下载（8 线程 Range），单线程自动回退 |
| HLS (`m3u8`) | 并发分片下载后拼接；主列表自动选最高码率；加密流回退 ffmpeg 拉流 |
| BitTorrent | magnet / `.torrent`，DHT + 公开 tracker，支持手动指定 peer |

- **粘贴播放地址 → 自动识别协议 → 自动命名 → 开始下载**（界面“粘贴并下载”按钮）
- **保存目录必须手动选择**，每个任务固定保存在 `所选目录\<资源同名目录>\`，不存在时自动创建
- 可选 **“下载后转为 MP4”**（HLS 拼出的 `.ts` 自动封装为 MP4）
- **P2P 加速服务器自动更新**：tracker 列表从 [ngosang/trackerslist](https://github.com/ngosang/trackerslist) 拉取，每 24 小时刷新一次，内置离线兜底列表，也可手动点“更新 P2P 服务器”
- 多任务并发（默认 2~3 个任务并行，任务内再并发分片）

## 转换（m3u8 目录）

- 选择源目录，递归扫描所有 `.m3u8`，按目录分批转换
- **输出文件名 = 源文件所在目录名**
- 同目录多个索引 → `目录名_索引名.mp4`
- 已存在同名 MP4 默认跳过，可勾选“覆盖已存在的 MP4”
- 三级降级：封装 → 视频复制+音频转 AAC → 完整重编码

## 其它

- 启动时检查 GitHub Release，有新版本提示下载（`-no-update-check` 可关闭）
- 自动探测 ffmpeg 并校验版本号（防止被替换成假壳）
- 任务日志：输出目录下 `task-yyyyMMdd-HHmmss.log`（UTF-8 BOM）

## 下载

- `HVConverter-Setup.exe`：安装版（含 ffmpeg，用户级安装、可卸载）
- `HVConverter-Portable.exe`：单文件免安装版（含 ffmpeg，首次运行解压到 `%LOCALAPPDATA%\HVConverter\cache`）

## 命令行

```powershell
# 下载（自动识别 m3u8 / mp4 / magnet / .torrent）
HVConverter-Portable.exe -dl "https://example.com/video/index.m3u8" -save "D:\Videos" -dl-convert

# 批量转换目录
HVConverter-Portable.exe -cli -src "D:\hls" -out "D:\out" -workers 3
```

参数：`-dl` 地址、`-save` 保存根目录、`-dl-convert` 下载后转换、`-dl-workers` 并发任务数、`-dl-peer` 手动 peer（`ip:port`）。

## 从源码构建

需要 Go 1.24+、[rsrc](https://github.com/akavel/rsrc)、ffmpeg 可执行文件。

```powershell
.\build.ps1 -Ffmpeg "C:\path\to\ffmpeg.exe"
```

产物：`dist\hvconverter.exe`、`dist\HVConverter-Portable.exe`、`dist\HVConverter-Setup.exe`

## 目录结构

```
hvconverter/      主程序（walk GUI + 下载引擎 + BT 客户端）
hvsetup/          安装器（含卸载清理助手）
build.ps1         一键构建
```

## 发布新版本

1. 改 `hvconverter/main.go` 里的 `appVersion`（例如 `1.1.1`）；
2. 提交并推同版本 tag：

```powershell
git commit -am "chore: bump v1.1.1"
git push origin master
git tag v1.1.1
git push origin v1.1.1
```

3. GitHub Actions 自动构建并创建 Release（无需 token）；
4. 用户端启动时自动检测到新版本。

## 免责声明

工具仅用于下载你有权保存的内容；DRM 保护的流媒体无法处理。请遵守内容平台的服务条款与当地法律。
