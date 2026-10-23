# HLSGo

HLSGo 是一个使用 Go 和 Fyne 编写的 Windows 图形界面视频下载工具。Go 下载核心负责处理 HLS 播放列表、并发下载分片、AES-128 解密、断点续传和分片合并；yt-dlp 负责解析网页及下载 Go 核心不支持的格式；FFmpeg 用于合并音视频流或封装视频。

请仅下载你有权保存的视频内容。DRM 加密内容不在支持范围内。

## 功能

- 批量粘贴链接并解析可用清晰度。
- 选择清晰度上限和保存目录。
- HLS 由 Go 核心下载，支持主播放列表清晰度选择、并发分片下载、重试、HTTP Range、AES-128-CBC 解密和分片续传。
- 非 HLS 格式由 yt-dlp 下载；需要时使用 FFmpeg 合并或重新封装。
- 下载队列支持暂停、继续、取消，并显示进度。
- 本地保存任务状态和下载历史；历史记录支持单条删除和全部清空。删除历史记录不会删除已下载的视频文件。
- 自动检查 yt-dlp 和 FFmpeg；缺少工具时可确认后下载到项目的 `bin` 目录。
- 支持配置 HTTP、HTTPS 和 SOCKS5 代理，也可以在设置页选择用户自行下载的工具目录。

## 下载方式说明

对于网页链接，HLSGo 调用 yt-dlp 获取视频信息和格式列表。可由 Go 处理的 HLS 格式使用 Go 分片下载核心；其他格式交给 yt-dlp 下载，并按选择的格式和清晰度处理。直接输入 `.m3u8` 地址时，Go 核心会解析播放列表并下载分片。

HLSGo 目前不支持需要 DRM 解密的内容。具体网站和格式是否可用，取决于 yt-dlp 的站点支持情况、视频访问权限及网络环境。

## 环境要求

- Windows
- Go 1.22 或更高版本
- Fyne 所需的 CGO 编译环境；Windows 下需要可用的 MinGW-w64 GCC 工具链
- yt-dlp 和 FFmpeg。首次启动时，程序会检查项目 `bin` 目录和 `PATH`；也会尝试查找通过 pip 安装的 yt-dlp。缺少工具时，程序会询问是否自动下载。

程序也支持在设置页指定本地 yt-dlp 或 FFmpeg 所在文件夹。

## 在 VS Code 中打开和运行

在 PowerShell 中进入项目目录：

```powershell
cd D:\.vscode\download_tools
```

运行图形界面：

```powershell
go run ./cmd/hlsgo
```

也可以在 VS Code 中打开项目目录，并通过已配置的 Go 启动项运行。第一次启动时，程序会检查 yt-dlp 和 FFmpeg；确认下载后，工具会放入项目的 `bin` 目录。

## 构建 Windows 图形界面程序

在项目根目录执行：

```powershell
go build -ldflags="-H windowsgui" -o HLSGo.exe .\cmd\hlsgo
```

`-H windowsgui` 会让 Windows 启动程序时不显示额外的命令行窗口。构建需要前述 Go、CGO 和 MinGW-w64 环境。

## 基本使用

1. 在下载页粘贴一个或多个视频网页链接或 `.m3u8` 地址。
2. 点击“解析链接”。
3. 为每条链接选择清晰度。
4. 设置保存目录后，点击“加入队列并开始下载”。
5. 在任务队列中查看进度并暂停、继续或取消任务；下载完成、失败或取消的任务会显示在下载历史页。

## 项目结构

```text
cmd/hlsgo/       程序入口
internal/ui/     Fyne 图形界面和本地任务记录
internal/tools/  yt-dlp、FFmpeg 检测、安装、解析和 yt-dlp 下载
internal/hls/    HLS 播放列表解析、分片下载、解密和合并
bin/             可选的本地 yt-dlp、FFmpeg 工具目录
```
