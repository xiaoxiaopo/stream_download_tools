# HLSGo

简体中文说明：[README.zh-CN.md](README.zh-CN.md)

Windows desktop video downloader written in Go with a Fyne graphical interface. The Go backend handles HLS playlist parsing, segment downloads, AES-128 decryption, resume state, and TS concatenation. FFmpeg handles final container remuxing.

## Current scope

- HLS media playlists and master playlist variant selection
- Concurrent segment downloads with retries and custom headers
- AES-128-CBC encrypted segments
- Persistent per-segment resume state
- TS concatenation and optional FFmpeg remux
- yt-dlp fallback downloads for formats the Go HLS core does not handle
- Batch queue, task pause/resume/cancel, progress, and saved download history

This is an initial functional core, not a complete port of every N_m3u8DL-CLI option. DRM-protected streams are out of scope. Use it only for streams you are authorized to download.

## Tool detection and installation

On startup, HLSGo checks `.\bin`, then `PATH`, for yt-dlp and FFmpeg. It also detects a yt-dlp Python package installed with pip by checking `py -3 -m yt_dlp`, `python -m yt_dlp`, and `python3 -m yt_dlp`. If a tool is missing, it asks before downloading it into `.\bin`.

The Windows yt-dlp standalone executable is downloaded from the official yt-dlp release. FFmpeg is downloaded as a Windows essentials ZIP build linked from the official FFmpeg download page, then only `ffmpeg.exe` is extracted. You can also install yt-dlp with pip; detection checks it by running `python -m yt_dlp --version` (or the equivalent `py` launcher). For a webpage URL, HLSGo asks yt-dlp for JSON metadata and available formats. HLS formats supported by the Go core use the Go segment downloader; other selected formats are downloaded by yt-dlp, with FFmpeg used to merge or remux when needed. Direct `.m3u8` URLs bypass yt-dlp and use the Go HLS downloader.

## Open and run in VS Code

```powershell
cd D:\.vscode\download_tools
go run ./cmd/hlsgo
```

Open `D:\.vscode\download_tools` as the VS Code folder, then press F5 and choose `HLSGo: 图形界面`. Paste one or more webpage or `.m3u8` URLs into the window, parse them, choose a quality, and add them to the queue. The first run checks for yt-dlp and FFmpeg and asks before downloading missing tools into `bin`.

The window includes a download queue, per-task pause/resume/cancel controls, progress, a saved history, output-folder selection, and tool status. Completed segments remain in each task's work directory and are reused when the task is resumed.

Fyne's desktop driver uses CGO on Windows. Build from a Go installation with a working MinGW-w64 GCC toolchain, as in the referenced TranslateTools project.
