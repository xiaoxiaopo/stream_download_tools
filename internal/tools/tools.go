package tools

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ytDlpURL      = "https://github.com/yt-dlp/yt-dlp/releases/latest/download/yt-dlp.exe"
	ytDlpPageURL  = "https://github.com/yt-dlp/yt-dlp/releases/latest"
	ffmpegZipURL  = "https://www.gyan.dev/ffmpeg/builds/ffmpeg-release-essentials.zip"
	ffmpegPageURL = "https://ffmpeg.org/download.html"
)

type Tool struct {
	Path       string
	PrefixArgs []string
	Proxy      string
}

type Set struct {
	YTDLP  Tool
	FFmpeg string
}

type DownloadProgress struct {
	ToolName   string
	DirectURL  string
	PageURL    string
	Stage      string
	Downloaded int64
	Total      int64
}

func Find(workingDir, ytOverride, ffmpegOverride string) Set {
	binDir := filepath.Join(workingDir, "bin")
	set := Set{YTDLP: findYTDLP(binDir)}
	if ytOverride != "" && verify(context.Background(), ytOverride, "--version") == nil {
		set.YTDLP = Tool{Path: ytOverride}
	}
	if ffmpegOverride != "" {
		if verify(context.Background(), ffmpegOverride, "-version") == nil {
			set.FFmpeg = ffmpegOverride
		}
	} else {
		set.FFmpeg = findExecutable(binDir, "ffmpeg.exe", "ffmpeg", "-version")
	}
	return set
}

// FindToolInFolder searches a selected folder (including common nested bin folders)
// for a runnable yt-dlp.exe or ffmpeg.exe.
func FindToolInFolder(folder, executable string) (string, error) {
	root, err := filepath.Abs(folder)
	if err != nil {
		return "", err
	}
	versionArg := "-version"
	if strings.EqualFold(executable, "yt-dlp.exe") {
		versionArg = "--version"
	}
	var found string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			rel, _ := filepath.Rel(root, path)
			if rel != "." && strings.Count(rel, string(filepath.Separator)) >= 5 {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.EqualFold(d.Name(), executable) && isUsable(path, versionArg) {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("在所选文件夹及其子文件夹中未找到可运行的 %s", executable)
	}
	return found, nil
}

type InstallError struct {
	ToolName, DirectURL, PageURL string
	Err                          error
}

func (e *InstallError) Error() string { return fmt.Sprintf("下载 %s 失败：%v", e.ToolName, e.Err) }
func (e *InstallError) Unwrap() error { return e.Err }
func DownloadURLs(toolName string) (string, string) {
	if strings.EqualFold(toolName, "yt-dlp") {
		return ytDlpURL, ytDlpPageURL
	}
	return ffmpegZipURL, ffmpegPageURL
}

func Missing(set Set) []string {
	var missing []string
	if set.YTDLP.Path == "" {
		missing = append(missing, "yt-dlp")
	}
	if set.FFmpeg == "" {
		missing = append(missing, "FFmpeg")
	}
	return missing
}

func InstallMissing(ctx context.Context, workingDir string, needYTDLP, needFFmpeg bool, proxy string, output io.Writer, onProgress func(DownloadProgress)) (Set, error) {
	if runtime.GOOS != "windows" {
		return Set{}, errors.New("automatic tool installation currently supports Windows only")
	}
	set := Find(workingDir, "", "")
	binDir := filepath.Join(workingDir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return set, err
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if strings.TrimSpace(proxy) != "" {
		u, err := url.Parse(strings.TrimSpace(proxy))
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
			return set, fmt.Errorf("代理地址无效：请使用 http://、https:// 或 socks5:// 地址")
		}
		transport.Proxy = http.ProxyURL(u)
	}
	client := &http.Client{Timeout: 20 * time.Minute, Transport: transport}
	if needYTDLP && set.YTDLP.Path == "" {
		dst := filepath.Join(binDir, "yt-dlp.exe")
		if err := downloadFile(ctx, client, ytDlpURL, dst, output, "yt-dlp", onProgress); err != nil {
			return set, &InstallError{ToolName: "yt-dlp", DirectURL: ytDlpURL, PageURL: ytDlpPageURL, Err: err}
		}
		if err := verify(ctx, dst, "--version"); err != nil {
			return set, fmt.Errorf("下载的 yt-dlp 无法运行：%w", err)
		}
		set.YTDLP = Tool{Path: dst}
	}
	if needFFmpeg && set.FFmpeg == "" {
		dst := filepath.Join(binDir, "ffmpeg.exe")
		if err := downloadFFmpeg(ctx, client, ffmpegZipURL, dst, output, onProgress); err != nil {
			return set, &InstallError{ToolName: "FFmpeg", DirectURL: ffmpegZipURL, PageURL: ffmpegPageURL, Err: err}
		}
		if err := verify(ctx, dst, "-version"); err != nil {
			return set, fmt.Errorf("下载的 FFmpeg 无法运行：%w", err)
		}
		set.FFmpeg = dst
	}
	return set, nil
}

// Ensure locates yt-dlp and FFmpeg, then asks before downloading any missing tool.
// The download location is <working directory>/bin.
func Ensure(ctx context.Context, workingDir, ffmpegOverride string, input io.Reader, output io.Writer) (Set, error) {
	if runtime.GOOS != "windows" {
		return Set{}, errors.New("automatic tool installation currently supports Windows only")
	}
	set := Find(workingDir, "", ffmpegOverride)
	if ffmpegOverride != "" && set.FFmpeg == "" {
		return set, fmt.Errorf("指定的 FFmpeg 不可运行")
	}
	missing := Missing(set)
	if len(missing) == 0 {
		return set, nil
	}

	binDir := filepath.Join(workingDir, "bin")
	fmt.Fprintf(output, "未找到：%s。是否下载到 %s？[y/N]: ", strings.Join(missing, "、"), binDir)
	answer, err := bufio.NewReader(input).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return set, err
	}
	answer = strings.TrimSpace(answer)
	if !strings.EqualFold(answer, "y") && !strings.EqualFold(answer, "yes") {
		return set, errors.New("用户选择不下载缺少的工具")
	}
	installed, err := InstallMissing(ctx, workingDir, set.YTDLP.Path == "", set.FFmpeg == "", "", output, nil)
	if err != nil {
		return installed, err
	}
	if ffmpegOverride != "" {
		installed.FFmpeg = ffmpegOverride
	}
	return installed, nil
}

func findYTDLP(binDir string) Tool {
	if p := findExecutable(binDir, "yt-dlp.exe", "yt-dlp", "--version"); p != "" {
		return Tool{Path: p}
	}
	// yt-dlp is also distributed as a Python package; support installations made with pip.
	for _, candidate := range []struct {
		name string
		args []string
	}{
		{"py", []string{"-3", "-m", "yt_dlp"}},
		{"python", []string{"-m", "yt_dlp"}},
		{"python3", []string{"-m", "yt_dlp"}},
	} {
		p, err := exec.LookPath(candidate.name)
		probeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		probeErr := error(nil)
		if err == nil {
			probeErr = verify(probeCtx, p, append(append([]string{}, candidate.args...), "--version")...)
		}
		cancel()
		if err == nil && probeErr == nil {
			return Tool{Path: p, PrefixArgs: candidate.args}
		}
	}
	return Tool{}
}

func findExecutable(binDir, localName, pathName, versionArg string) string {
	if local, err := FindToolInFolder(binDir, localName); err == nil {
		return local
	}
	if path, err := exec.LookPath(pathName); err == nil && isUsable(path, versionArg) {
		return path
	}
	return ""
}

func isUsable(path, versionArg string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return verify(ctx, path, versionArg) == nil
}

func verify(ctx context.Context, path string, args ...string) error {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run()
}

type StreamInfo struct {
	URL      string
	Title    string
	FormatID string
	Height   int
}

type FormatChoice struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Height      int    `json:"height"`
	URL         string `json:"url,omitempty"`
	Protocol    string `json:"protocol,omitempty"`
	Supported   bool   `json:"supported"`
	YTDLPFormat string `json:"yt_dlp_format,omitempty"`
}

type extractorResult struct {
	Title   string   `json:"title"`
	URL     string   `json:"url"`
	Formats []format `json:"formats"`
}

type format struct {
	URL            string  `json:"url"`
	FormatID       string  `json:"format_id"`
	Protocol       string  `json:"protocol"`
	Height         int     `json:"height"`
	FileSize       int64   `json:"filesize"`
	FileSizeApprox int64   `json:"filesize_approx"`
	TBR            float64 `json:"tbr"`
	VCodec         string  `json:"vcodec"`
	ACodec         string  `json:"acodec"`
	Ext            string  `json:"ext"`
}

// ResolveHLS asks yt-dlp for metadata and selects the best combined A/V HLS format.
// Direct m3u8 URLs are returned unchanged so the Go parser handles their variants.
func ResolveHLS(ctx context.Context, yt Tool, sourceURL string, maxHeight int) (StreamInfo, bool, error) {
	choices, title, direct, err := ListHLSFormats(ctx, yt, sourceURL, maxHeight)
	if err != nil {
		return StreamInfo{}, false, err
	}
	if direct {
		return StreamInfo{URL: sourceURL}, false, nil
	}
	if len(choices) == 0 {
		return StreamInfo{}, false, errors.New("yt-dlp 没有提供可供 Go 下载器处理的 HLS 音视频合并流")
	}
	best := choices[0]
	return StreamInfo{URL: best.URL, Title: title, FormatID: best.ID, Height: best.Height}, true, nil
}

func ListHLSFormats(ctx context.Context, yt Tool, sourceURL string, maxHeight int) ([]FormatChoice, string, bool, error) {
	parsed, err := url.Parse(sourceURL)
	if err != nil {
		return nil, "", false, err
	}
	if strings.EqualFold(filepath.Ext(parsed.Path), ".m3u8") {
		return []FormatChoice{{ID: "auto", Label: "自动选择清晰度", Height: 0, URL: sourceURL, Protocol: "m3u8", Supported: true}}, "", true, nil
	}
	if yt.Path == "" {
		return nil, "", false, errors.New("yt-dlp is unavailable")
	}
	args := append([]string{}, yt.PrefixArgs...)
	args = append(args, "--dump-single-json", "--no-warnings", "--no-playlist", "--skip-download", "--no-update")
	if strings.TrimSpace(yt.Proxy) != "" {
		args = append(args, "--proxy", strings.TrimSpace(yt.Proxy))
	}
	args = append(args, sourceURL)
	cmd := exec.CommandContext(ctx, yt.Path, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		return nil, "", false, fmt.Errorf("yt-dlp 解析失败：%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var info extractorResult
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, "", false, fmt.Errorf("解析 yt-dlp 输出失败：%w", err)
	}
	choices := make([]FormatChoice, 0)
	for _, f := range info.Formats {
		if f.URL == "" || !isHLSFormat(f) || f.VCodec == "none" || f.VCodec == "" || f.ACodec == "none" || f.ACodec == "" {
			continue
		}
		if maxHeight > 0 && f.Height > maxHeight {
			continue
		}
		label := fmt.Sprintf("%dp · %s · %s", f.Height, f.Ext, f.FormatID)
		if f.Height == 0 {
			label = fmt.Sprintf("%s · %s", f.Ext, f.FormatID)
		}
		choices = append(choices, FormatChoice{ID: f.FormatID, Label: label, Height: f.Height, URL: f.URL, Protocol: f.Protocol, Supported: true})
	}
	if len(choices) == 0 {
		// Match the Python tool's direct URL selection: prefer progressive formats,
		// then fall back to a video-only stream when no combined format exists.
		var progressive, videoOnly []format
		for _, f := range info.Formats {
			if f.URL == "" || f.VCodec == "none" || f.VCodec == "" || (maxHeight > 0 && f.Height > maxHeight) {
				continue
			}
			videoOnly = append(videoOnly, f)
			if f.ACodec != "none" && f.ACodec != "" {
				progressive = append(progressive, f)
			}
		}
		fallback := progressive
		if len(fallback) == 0 {
			fallback = videoOnly
		}
		sort.SliceStable(fallback, func(i, j int) bool {
			if fallback[i].Height != fallback[j].Height {
				return fallback[i].Height < fallback[j].Height
			}
			size := func(f format) int64 {
				if f.FileSize > 0 {
					return f.FileSize
				}
				return f.FileSizeApprox
			}
			if size(fallback[i]) != size(fallback[j]) {
				return size(fallback[i]) < size(fallback[j])
			}
			return fallback[i].FormatID < fallback[j].FormatID
		})
		for _, f := range fallback {
			protocol := f.Protocol
			if protocol == "" {
				if parsedURL, parseErr := url.Parse(f.URL); parseErr == nil {
					protocol = parsedURL.Scheme
				}
			}
			ytDlpFormat := f.FormatID
			if f.ACodec == "none" || f.ACodec == "" {
				ytDlpFormat = f.FormatID + "+bestaudio/" + f.FormatID
			}
			label := fmt.Sprintf("直链 · %dp · %s · %s（yt-dlp下载）", f.Height, protocol, f.FormatID)
			if f.Height == 0 {
				label = fmt.Sprintf("直链 · %s · %s（yt-dlp下载）", protocol, f.FormatID)
			}
			choices = append(choices, FormatChoice{ID: f.FormatID, Label: label, Height: f.Height, URL: f.URL, Protocol: protocol, YTDLPFormat: ytDlpFormat})
		}
	}
	if len(choices) == 0 && info.URL != "" {
		if isM3U8(info.URL) {
			return []FormatChoice{{ID: "auto", Label: "自动选择清晰度", URL: info.URL, Protocol: "m3u8", Supported: true}}, info.Title, false, nil
		}
		return []FormatChoice{{ID: "direct", Label: "直链（yt-dlp下载）", URL: info.URL, Protocol: "direct", YTDLPFormat: bestYTDLPFormat(maxHeight)}}, info.Title, false, nil
	}
	return choices, info.Title, false, nil
}

func ResolveHLSFormat(ctx context.Context, yt Tool, sourceURL, formatID string, maxHeight int) (StreamInfo, error) {
	choices, title, direct, err := ListHLSFormats(ctx, yt, sourceURL, maxHeight)
	if err != nil {
		return StreamInfo{}, err
	}
	if direct {
		return StreamInfo{URL: sourceURL}, nil
	}
	for _, choice := range choices {
		if choice.ID == formatID || formatID == "auto" {
			return StreamInfo{URL: choice.URL, Title: title, FormatID: choice.ID, Height: choice.Height}, nil
		}
	}
	return StreamInfo{}, fmt.Errorf("清晰度 %q 不存在或超出高度限制", formatID)
}

func bestYTDLPFormat(maxHeight int) string {
	if maxHeight > 0 {
		return fmt.Sprintf("bestvideo[height<=%d]+bestaudio/best[height<=%d]", maxHeight, maxHeight)
	}
	return "bv*+ba/b"
}

var ytProgressPattern = regexp.MustCompile(`\[download\]\s+([0-9]+(?:\.[0-9]+)?)% of\s+~?([0-9]+(?:\.[0-9]+)?)\s*(B|KiB|MiB|GiB|TiB|KB|MB|GB|TB)`)

// DownloadWithYTDLP downloads non-HLS formats through yt-dlp while leaving
// playlist/segment handling and remuxing to yt-dlp and FFmpeg.
func DownloadWithYTDLP(ctx context.Context, yt Tool, sourceURL, selector, output, ffmpegPath string, maxHeight int, onProgress func(done, total int64)) error {
	if yt.Path == "" {
		return errors.New("yt-dlp 不可用，请在设置中选择 yt-dlp 文件夹")
	}
	if _, err := os.Stat(output); err == nil {
		return fmt.Errorf("输出文件已存在，为保护已有文件，本次下载未覆盖：%s", output)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("检查输出文件失败：%w", err)
	}
	if strings.TrimSpace(selector) == "" {
		selector = bestYTDLPFormat(maxHeight)
	}
	args := append([]string{}, yt.PrefixArgs...)
	args = append(args, "--no-playlist", "--no-update", "--no-overwrites", "--continue", "--newline", "--progress", "--retries", "10", "--fragment-retries", "10", "--format", selector, "--output", output, "--merge-output-format", "mp4", "--remux-video", "mp4")
	if strings.TrimSpace(yt.Proxy) != "" {
		args = append(args, "--proxy", strings.TrimSpace(yt.Proxy))
	}
	if strings.TrimSpace(ffmpegPath) != "" {
		args = append(args, "--ffmpeg-location", filepath.Dir(ffmpegPath))
	}
	args = append(args, sourceURL)
	cmd := exec.CommandContext(ctx, yt.Path, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 yt-dlp 失败：%w", err)
	}
	type outputLine struct {
		text   string
		stderr bool
	}
	lines := make(chan outputLine, 128)
	var readers sync.WaitGroup
	readLines := func(reader io.Reader, isStderr bool) {
		defer readers.Done()
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			lines <- outputLine{text: scanner.Text(), stderr: isStderr}
		}
	}
	readers.Add(2)
	go readLines(stdout, false)
	go readLines(stderr, true)
	go func() { readers.Wait(); close(lines) }()
	var stderrText strings.Builder
	for line := range lines {
		if line.stderr {
			stderrText.WriteString(line.text)
			stderrText.WriteByte('\n')
		}
		if done, total, ok := parseYTDLPProgress(line.text); ok && onProgress != nil {
			onProgress(done, total)
		}
	}
	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if waitErr != nil {
		return fmt.Errorf("yt-dlp 下载失败：%w: %s", waitErr, strings.TrimSpace(stderrText.String()))
	}
	return nil
}

func parseYTDLPProgress(line string) (int64, int64, bool) {
	m := ytProgressPattern.FindStringSubmatch(line)
	if len(m) != 4 {
		return 0, 0, false
	}
	pct, err1 := strconv.ParseFloat(m[1], 64)
	size, err2 := strconv.ParseFloat(m[2], 64)
	if err1 != nil || err2 != nil || pct < 0 {
		return 0, 0, false
	}
	unit := strings.ToUpper(m[3])
	multiplier := int64(1000)
	if strings.Contains(m[3], "i") {
		multiplier = 1024
	}
	units := map[string]int{"B": 0, "KB": 1, "MB": 2, "GB": 3, "TB": 4, "KIB": 1, "MIB": 2, "GIB": 3, "TIB": 4}
	power, ok := units[unit]
	if !ok {
		return 0, 0, false
	}
	factor := int64(1)
	for i := 0; i < power; i++ {
		factor *= multiplier
	}
	total := int64(size * float64(factor))
	done := int64(float64(total) * pct / 100)
	return done, total, true
}

func isHLSFormat(f format) bool {
	protocol := strings.ToLower(f.Protocol)
	return strings.Contains(protocol, "m3u8") || isM3U8(f.URL)
}

func isM3U8(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && strings.EqualFold(filepath.Ext(u.Path), ".m3u8")
}

func downloadFile(ctx context.Context, client *http.Client, source, destination string, output io.Writer, toolName string, onProgress func(DownloadProgress)) error {
	directURL, pageURL := DownloadURLs(toolName)
	if onProgress != nil {
		onProgress(DownloadProgress{ToolName: toolName, DirectURL: directURL, PageURL: pageURL, Stage: "正在连接下载服务器…"})
	}
	tmp := destination + ".download"
	_ = os.Remove(tmp)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	if onProgress != nil {
		onProgress(DownloadProgress{ToolName: toolName, DirectURL: directURL, PageURL: pageURL, Stage: "正在下载…", Total: resp.ContentLength})
	}
	file, err := os.Create(tmp)
	if err != nil {
		return err
	}
	var copyErr error
	copyErr = copyWithProgress(file, resp.Body, resp.ContentLength, output, toolName, directURL, pageURL, onProgress)
	closeErr := file.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	return replace(tmp, destination)
}

func copyWithProgress(dst io.Writer, src io.Reader, total int64, output io.Writer, toolName, directURL, pageURL string, onProgress func(DownloadProgress)) error {
	buf := make([]byte, 128*1024)
	var copied int64
	lastReport := time.Now()
	for {
		n, err := src.Read(buf)
		if n > 0 {
			written, writeErr := dst.Write(buf[:n])
			copied += int64(written)
			if writeErr != nil {
				return writeErr
			}
			if onProgress != nil {
				onProgress(DownloadProgress{ToolName: toolName, DirectURL: directURL, PageURL: pageURL, Stage: "正在下载…", Downloaded: copied, Total: total})
			}
			if time.Since(lastReport) >= 750*time.Millisecond {
				if total > 0 {
					fmt.Fprintf(output, "\r下载中：%3.0f%%", 100*float64(copied)/float64(total))
				} else {
					fmt.Fprintf(output, "\r已下载：%s", humanBytes(copied))
				}
				lastReport = time.Now()
			}
		}
		if err == io.EOF {
			if onProgress != nil {
				onProgress(DownloadProgress{ToolName: toolName, DirectURL: directURL, PageURL: pageURL, Stage: "下载完成", Downloaded: copied, Total: total})
			}
			fmt.Fprintln(output, "\r下载完成      ")
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func humanBytes(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	value := float64(size)
	for _, suffix := range []string{"KB", "MB", "GB", "TB"} {
		value /= unit
		if value < unit || suffix == "TB" {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%d B", size)
}

func downloadFFmpeg(ctx context.Context, client *http.Client, source, destination string, output io.Writer, onProgress func(DownloadProgress)) error {
	archivePath := destination + ".zip"
	defer os.Remove(archivePath)
	if err := downloadFile(ctx, client, source, archivePath, output, "FFmpeg", onProgress); err != nil {
		return err
	}
	if onProgress != nil {
		onProgress(DownloadProgress{ToolName: "FFmpeg", DirectURL: ffmpegZipURL, PageURL: ffmpegPageURL, Stage: "下载完成，正在解压 FFmpeg…", Downloaded: 1, Total: 1})
	}
	archive, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer archive.Close()
	for _, entry := range archive.File {
		if !strings.HasSuffix(strings.ToLower(filepath.ToSlash(entry.Name)), "/bin/ffmpeg.exe") {
			continue
		}
		if entry.UncompressedSize64 > 500*1024*1024 {
			return errors.New("ffmpeg.exe archive entry is unexpectedly large")
		}
		r, err := entry.Open()
		if err != nil {
			return err
		}
		tmp := destination + ".download"
		f, err := os.Create(tmp)
		if err != nil {
			r.Close()
			return err
		}
		_, copyErr := io.Copy(f, r)
		closeRErr := r.Close()
		closeFErr := f.Close()
		if copyErr != nil {
			_ = os.Remove(tmp)
			return copyErr
		}
		if closeRErr != nil {
			_ = os.Remove(tmp)
			return closeRErr
		}
		if closeFErr != nil {
			_ = os.Remove(tmp)
			return closeFErr
		}
		return replace(tmp, destination)
	}
	return errors.New("ffmpeg.exe was not found in the downloaded ZIP archive")
}

func replace(src, dst string) error {
	if err := os.Remove(dst); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(src, dst)
}
