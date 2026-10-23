package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/local/hlsgo/internal/hls"
	"github.com/local/hlsgo/internal/tools"
)

type task struct {
	ID          string    `json:"id"`
	Source      string    `json:"source"`
	Title       string    `json:"title"`
	FormatID    string    `json:"format_id"`
	FormatName  string    `json:"format_name"`
	UseYTDLP    bool      `json:"use_yt_dlp,omitempty"`
	YTDLPFormat string    `json:"yt_dlp_format,omitempty"`
	Height      int       `json:"height"`
	MaxHeight   int       `json:"max_height"`
	Output      string    `json:"output"`
	Status      string    `json:"status"`
	Done        int       `json:"done"`
	Total       int       `json:"total"`
	Error       string    `json:"error,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type parsedItem struct {
	Source   string
	Title    string
	Choices  []tools.FormatChoice
	Selected string
	Err      string
}

type appState struct {
	Tasks      []task `json:"tasks"`
	OutputDir  string `json:"output_dir"`
	MaxHeight  int    `json:"max_height"`
	YTDLPPath  string `json:"yt_dlp_path,omitempty"`
	FFmpegPath string `json:"ffmpeg_path,omitempty"`
	ProxyURL   string `json:"proxy_url,omitempty"`
}

type App struct {
	fyneApp   fyne.App
	window    fyne.Window
	ctx       context.Context
	rootDir   string
	statePath string

	mu            sync.Mutex
	state         appState
	toolSet       tools.Set
	parseItems    []parsedItem
	activeID      string
	activeCancel  context.CancelFunc
	workerRunning bool

	linksEntry      *widget.Entry
	outputEntry     *widget.Entry
	heightSelect    *widget.Select
	parseStatus     *widget.Label
	parsedList      *fyne.Container
	taskList        *fyne.Container
	historyList     *fyne.Container
	toolStatus      *widget.Label
	parseButton     *widget.Button
	ytPathEntry     *widget.Entry
	ffmpegPathEntry *widget.Entry
	proxyEntry      *widget.Entry
}

func Run() {
	a := app.NewWithID("local.hlsgo.downloader")
	win := a.NewWindow("HLSGo 视频下载器")
	win.Resize(fyne.NewSize(1040, 760))
	win.CenterOnScreen()
	ui := newApp(a, win)
	win.SetContent(ui.build())
	ui.refreshTasks()
	win.Show()
	ui.checkTools()
	a.Run()
}

func newApp(a fyne.App, win fyne.Window) *App {
	root, _ := os.Getwd()
	root = findProjectRoot(root)
	configDir, err := os.UserConfigDir()
	if err != nil {
		configDir = root
	}
	configDir = filepath.Join(configDir, "HLSGo")
	_ = os.MkdirAll(configDir, 0755)
	u := &App{fyneApp: a, window: win, rootDir: root, statePath: filepath.Join(configDir, "tasks.json")}
	u.state.OutputDir = filepath.Join(root, "downloads")
	u.state.MaxHeight = 0
	u.loadState()
	return u
}

func findProjectRoot(start string) string {
	current, err := filepath.Abs(start)
	if err != nil {
		return start
	}
	for {
		if _, err := os.Stat(filepath.Join(current, "go.mod")); err == nil {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			return start
		}
		current = parent
	}
}

func (a *App) build() fyne.CanvasObject {
	icon := widget.NewIcon(theme.DownloadIcon())
	title := widget.NewLabelWithStyle("HLSGo", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	caption := widget.NewLabel("Go 分片下载 · yt-dlp 解析 · FFmpeg 封装")
	header := container.NewHBox(icon, container.NewVBox(title, caption), layout.NewSpacer())

	queueTab := container.NewTabItem("下载队列", a.buildDownloadPage())
	historyTab := container.NewTabItem("下载历史", a.buildHistoryPage())
	settingsTab := container.NewTabItem("设置", a.buildSettingsPage())
	tabs := container.NewAppTabs(queueTab, historyTab, settingsTab)
	return container.NewBorder(header, widget.NewLabel("仅下载你有权保存的视频内容"), nil, nil, tabs)
}

func (a *App) buildDownloadPage() fyne.CanvasObject {
	a.linksEntry = widget.NewMultiLineEntry()
	a.linksEntry.SetPlaceHolder("每行粘贴一个视频网页链接或 m3u8 地址")
	a.linksEntry.SetMinRowsVisible(3)
	a.linksEntry.Wrapping = fyne.TextWrapWord
	a.heightSelect = widget.NewSelect([]string{"自动最高", "1080p", "720p", "480p", "360p"}, func(value string) {
		a.mu.Lock()
		a.state.MaxHeight = parseHeight(value)
		a.saveLocked()
		a.mu.Unlock()
	})
	a.heightSelect.SetSelected(heightLabel(a.state.MaxHeight))
	a.outputEntry = widget.NewEntry()
	a.outputEntry.SetText(a.state.OutputDir)
	browse := widget.NewButton("选择目录…", func() {
		a.chooseFolder("选择视频保存目录", func(path string) {
			a.outputEntry.SetText(path)
			a.mu.Lock()
			a.state.OutputDir = path
			a.saveLocked()
			a.mu.Unlock()
		})
	})
	a.parseStatus = widget.NewLabel("粘贴链接后解析可用清晰度。")
	a.parsedList = container.NewVBox()
	a.parseButton = widget.NewButtonWithIcon("解析链接", theme.SearchIcon(), a.parseLinks)
	addButton := widget.NewButtonWithIcon("加入队列并开始下载", theme.ContentAddIcon(), a.addParsedTasks)
	inputCard := widget.NewCard("添加下载任务", "粘贴链接后解析，选择清晰度，再点击“加入队列并开始下载”", container.NewVBox(
		a.linksEntry,
		container.NewGridWithColumns(2, container.NewVBox(widget.NewLabel("清晰度上限"), a.heightSelect), container.NewVBox(widget.NewLabel("保存目录"), container.NewBorder(nil, nil, nil, browse, a.outputEntry))),
		container.NewHBox(a.parseButton, addButton, a.parseStatus),
	))
	parsedCard := widget.NewCard("解析结果", "为每条链接选择清晰度", container.NewVBox(a.parsedList))
	a.taskList = container.NewVBox()
	taskCard := widget.NewCard("任务队列", "暂停后可继续；关闭程序后可恢复未完成分片", container.NewVBox(a.taskList))
	body := container.NewVBox(inputCard, parsedCard, taskCard)
	return container.NewVScroll(body)
}

func (a *App) buildHistoryPage() fyne.CanvasObject {
	a.historyList = container.NewVBox()
	clearButton := widget.NewButton("清空全部历史", func() {
		dialog.NewConfirm("清空下载历史", "确定删除所有已完成、失败和已取消的历史记录吗？此操作不会删除已下载的视频文件。", func(ok bool) {
			if ok {
				a.clearHistory()
			}
		}, a.window).Show()
	})
	header := container.NewBorder(nil, nil, widget.NewLabel("已完成、失败和已取消的任务会保存在本机。"), clearButton)
	return container.NewBorder(header, nil, nil, nil, container.NewVScroll(a.historyList))
}

func (a *App) buildSettingsPage() fyne.CanvasObject {
	a.toolStatus = widget.NewLabel("正在检查 yt-dlp 和 FFmpeg…")
	install := widget.NewButton("检查 / 安装工具", a.checkTools)
	a.ytPathEntry = widget.NewEntry()
	a.ytPathEntry.SetText(a.state.YTDLPPath)
	a.ytPathEntry.SetPlaceHolder("选择包含 yt-dlp.exe 的文件夹")
	a.ffmpegPathEntry = widget.NewEntry()
	a.ffmpegPathEntry.SetText(a.state.FFmpegPath)
	a.ffmpegPathEntry.SetPlaceHolder("选择包含 ffmpeg.exe 的文件夹")
	a.proxyEntry = widget.NewEntry()
	a.proxyEntry.SetText(a.state.ProxyURL)
	a.proxyEntry.SetPlaceHolder("例如 http://127.0.0.1:7890 或 socks5://127.0.0.1:1080")
	chooseToolFolder := func(title, executable string, entry *widget.Entry, store func(string)) {
		a.chooseFolder(title, func(folder string) {
			found, err := tools.FindToolInFolder(folder, executable)
			if err != nil {
				dialog.ShowError(err, a.window)
				return
			}
			entry.SetText(found)
			store(found)
			a.checkTools()
		})
	}
	chooseYt := widget.NewButton("选择文件夹…", func() {
		chooseToolFolder("选择 yt-dlp 文件夹", "yt-dlp.exe", a.ytPathEntry, func(p string) { a.mu.Lock(); a.state.YTDLPPath = p; a.saveLocked(); a.mu.Unlock() })
	})
	chooseFF := widget.NewButton("选择文件夹…", func() {
		chooseToolFolder("选择 FFmpeg 文件夹", "ffmpeg.exe", a.ffmpegPathEntry, func(p string) { a.mu.Lock(); a.state.FFmpegPath = p; a.saveLocked(); a.mu.Unlock() })
	})
	saveProxy := widget.NewButton("保存代理", func() {
		value := strings.TrimSpace(a.proxyEntry.Text)
		if value != "" {
			u, err := url.Parse(value)
			if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
				dialog.ShowError(fmt.Errorf("代理地址无效。请使用 http://、https:// 或 socks5:// 地址"), a.window)
				return
			}
		}
		a.mu.Lock()
		a.state.ProxyURL = value
		a.saveLocked()
		a.mu.Unlock()
		dialog.ShowInformation("已保存", "代理设置将用于工具下载、yt-dlp 解析和视频分片请求。", a.window)
	})
	openDownloads := widget.NewButton("打开保存目录", func() {
		_ = os.MkdirAll(a.state.OutputDir, 0755)
		_ = exec.Command("explorer.exe", a.state.OutputDir).Start()
	})
	return container.NewVBox(
		widget.NewCard("外部工具", "优先使用下方指定程序、项目 bin 或 PATH 中的程序；yt-dlp 也支持 Python pip 安装。", container.NewVBox(a.toolStatus,
			container.NewBorder(nil, nil, widget.NewLabel("yt-dlp 文件夹"), chooseYt, a.ytPathEntry),
			container.NewBorder(nil, nil, widget.NewLabel("FFmpeg 文件夹"), chooseFF, a.ffmpegPathEntry),
			container.NewHBox(install, openDownloads))),
		widget.NewCard("网络代理", "支持 HTTP、HTTPS 和 SOCKS5 代理。", container.NewVBox(container.NewBorder(nil, nil, widget.NewLabel("代理地址"), saveProxy, a.proxyEntry))),
		widget.NewLabel("清晰度上限与保存目录可在下载页设置。"))
}

func (a *App) chooseFolder(title string, selected func(string)) {
	go func() {
		path, err := pickNativeFolder(title)
		fyne.Do(func() {
			if err != nil {
				dialog.ShowError(err, a.window)
				return
			}
			if path != "" && selected != nil {
				selected(path)
			}
		})
	}()
}

func (a *App) checkTools() {
	a.mu.Lock()
	ytPath, ffmpegPath, proxyURL := a.state.YTDLPPath, a.state.FFmpegPath, a.state.ProxyURL
	a.mu.Unlock()
	set := tools.Find(a.rootDir, ytPath, ffmpegPath)
	set.YTDLP.Proxy = proxyURL
	a.mu.Lock()
	a.toolSet = set
	a.mu.Unlock()
	a.updateToolStatus()
	missing := tools.Missing(set)
	if len(missing) == 0 {
		return
	}
	dialog.NewConfirm("需要下载工具", fmt.Sprintf("未找到 %s。是否下载到项目目录的 bin 文件夹？", strings.Join(missing, "、")), func(ok bool) {
		if !ok {
			return
		}
		installCtx, cancelInstall := context.WithCancel(context.Background())
		progressLabel := widget.NewLabel("正在准备下载…")
		progressBar := widget.NewProgressBar()
		stallWarning := widget.NewLabel("网络暂时没有下载进展。可能是连接超时或网络受阻；可以使用直链下载，或取消本次下载。")
		stallWarning.Hide()
		downloadLink := widget.NewHyperlink("打开工具下载地址", nil)
		downloadLink.Hide()
		downloadEntry := widget.NewEntry()
		downloadEntry.SetPlaceHolder("下载直链")
		downloadEntry.Hide()
		cancelButton := widget.NewButton("取消下载", cancelInstall)
		var activityMu sync.Mutex
		lastNetworkActivity := time.Now()
		stage := "准备下载"
		currentTool := ""
		var previousBytes int64
		watchStop := make(chan struct{})
		content := container.NewVBox(progressLabel, progressBar, stallWarning, downloadLink, downloadEntry, cancelButton)
		progressDialog := dialog.NewCustomWithoutButtons("下载工具", content, a.window)
		progressDialog.Show()
		go func() {
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			warned := false
			for {
				select {
				case <-watchStop:
					return
				case now := <-ticker.C:
					activityMu.Lock()
					idleFor := now.Sub(lastNetworkActivity)
					watchStage := stage
					activityMu.Unlock()
					if !warned && (watchStage == "正在连接下载服务器…" || watchStage == "正在下载…") && idleFor >= 5*time.Second {
						warned = true
						fyne.Do(func() {
							stallWarning.Show()
							downloadLink.Show()
							downloadEntry.Show()
						})
					}
				}
			}
		}()
		go func() {
			set, err := tools.InstallMissing(installCtx, a.rootDir, set.YTDLP.Path == "", set.FFmpeg == "", proxyURL, &discardWriter{}, func(p tools.DownloadProgress) {
				activityMu.Lock()
				if p.ToolName != currentTool || ((p.Stage == "正在连接下载服务器…" || p.Stage == "正在下载…") && stage != p.Stage) {
					currentTool = p.ToolName
					previousBytes = 0
					lastNetworkActivity = time.Now()
				}
				if p.Downloaded > previousBytes {
					lastNetworkActivity = time.Now()
				}
				previousBytes = p.Downloaded
				stage = p.Stage
				activityMu.Unlock()
				fyne.Do(func() {
					progressLabel.SetText(p.ToolName + "：" + p.Stage)
					if p.Total > 0 {
						progressBar.SetValue(float64(p.Downloaded) / float64(p.Total))
						progressLabel.SetText(fmt.Sprintf("%s：%s  (%s / %s)", p.ToolName, p.Stage, formatBytes(p.Downloaded), formatBytes(p.Total)))
					} else if p.Downloaded > 0 {
						progressLabel.SetText(fmt.Sprintf("%s：%s  (%s)", p.ToolName, p.Stage, formatBytes(p.Downloaded)))
					}
					if p.Stage == "下载完成，正在解压 FFmpeg…" {
						progressBar.SetValue(1)
					}
					if directURL, parseErr := url.Parse(p.DirectURL); parseErr == nil {
						downloadLink.SetText("打开 " + p.ToolName + " 下载地址")
						downloadLink.SetURL(directURL)
						downloadEntry.SetText(p.DirectURL)
					}
				})
			})
			close(watchStop)
			cancelInstall()
			fyne.Do(func() {
				progressDialog.Hide()
				if err != nil {
					if errors.Is(err, context.Canceled) {
						return
					}
					a.showInstallError(err)
					return
				}
				if ytPath != "" {
					set.YTDLP = tools.Tool{Path: ytPath, Proxy: proxyURL}
				}
				if ffmpegPath != "" {
					set.FFmpeg = ffmpegPath
				}
				set.YTDLP.Proxy = proxyURL
				a.mu.Lock()
				a.toolSet = set
				a.mu.Unlock()
				a.updateToolStatus()
				dialog.ShowInformation("工具已准备好", "yt-dlp 和 FFmpeg 已可用。", a.window)
			})
		}()
	}, a.window).Show()
}

func formatBytes(size int64) string {
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

func (a *App) showInstallError(err error) {
	var installErr *tools.InstallError
	if !errors.As(err, &installErr) {
		dialog.ShowError(err, a.window)
		return
	}
	linkURL, parseErr := url.Parse(installErr.PageURL)
	if parseErr != nil {
		dialog.ShowError(err, a.window)
		return
	}
	downloadURL, parseErr := url.Parse(installErr.DirectURL)
	if parseErr != nil {
		dialog.ShowError(err, a.window)
		return
	}
	direct := widget.NewEntry()
	direct.SetText(installErr.DirectURL)
	content := container.NewVBox(widget.NewLabel(fmt.Sprintf("%s 自动下载失败：\n%s\n\n可以通过浏览器或其他下载工具获取文件；完成后在设置页选择文件夹。", installErr.ToolName, installErr.Err)), widget.NewHyperlink("直接下载", downloadURL), widget.NewHyperlink("打开官方发布页面", linkURL), widget.NewLabel("下载地址（可选择并复制）："), direct)
	dialog.NewCustom(installErr.ToolName+" 下载失败", "关闭", content, a.window).Show()
}

type discardWriter struct{}

func (*discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func (a *App) updateToolStatus() {
	if a.toolStatus == nil {
		return
	}
	a.mu.Lock()
	set := a.toolSet
	a.mu.Unlock()
	yt := "未找到"
	if set.YTDLP.Path != "" {
		yt = set.YTDLP.Path
	}
	ff := "未找到"
	if set.FFmpeg != "" {
		ff = set.FFmpeg
	}
	a.toolStatus.SetText(fmt.Sprintf("yt-dlp：%s\nFFmpeg：%s", yt, ff))
}

func (a *App) parseLinks() {
	urls := splitURLs(a.linksEntry.Text)
	if len(urls) == 0 {
		dialog.ShowInformation("没有链接", "请先粘贴至少一个视频链接。", a.window)
		return
	}
	a.parseButton.Disable()
	a.parseStatus.SetText("正在解析…")
	a.mu.Lock()
	maxHeight := a.state.MaxHeight
	toolSet := a.toolSet
	toolSet.YTDLP.Proxy = a.state.ProxyURL
	a.mu.Unlock()
	go func() {
		items := make([]parsedItem, 0, len(urls))
		for _, source := range urls {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			choices, title, _, err := tools.ListHLSFormats(ctx, toolSet.YTDLP, source, maxHeight)
			cancel()
			item := parsedItem{Source: source, Title: title, Choices: choices}
			if err != nil {
				item.Err = err.Error()
			} else if len(choices) == 0 {
				item.Err = "没有解析到可用的视频格式。请检查链接是否可访问，或该网站是否受 yt-dlp 支持。"
			} else if len(choices) > 0 {
				item.Selected = choices[len(choices)-1].ID
			}
			items = append(items, item)
		}
		fyne.Do(func() {
			a.parseItems = items
			a.renderParsed()
			a.parseButton.Enable()
			a.parseStatus.SetText(fmt.Sprintf("解析完成：%d 条", len(items)))
		})
	}()
}

func (a *App) renderParsed() {
	objects := make([]fyne.CanvasObject, 0, len(a.parseItems))
	for i := range a.parseItems {
		item := &a.parseItems[i]
		title := item.Title
		if title == "" {
			title = item.Source
		}
		if item.Err != "" {
			objects = append(objects, widget.NewCard(title, "无法加入下载队列", container.NewVBox(widget.NewLabel(item.Err), widget.NewLabel("原始链接："+item.Source))))
			continue
		}
		labels := make([]string, 0, len(item.Choices))
		ids := make(map[string]string)
		for _, choice := range item.Choices {
			labels = append(labels, choice.Label)
			ids[choice.Label] = choice.ID
		}
		streamEntry := widget.NewEntry()
		streamEntry.SetPlaceHolder("选择清晰度后显示视频流地址")
		supportNote := widget.NewLabel("")
		supportNote.Hide()
		showChoice := func(id string) {
			for _, choice := range item.Choices {
				if choice.ID != id {
					continue
				}
				streamEntry.SetText(choice.URL)
				if choice.Supported {
					supportNote.Hide()
				} else {
					supportNote.SetText("此格式由 yt-dlp 下载，并由 FFmpeg 合并或封装。")
					supportNote.Show()
				}
				return
			}
		}
		selectFormat := widget.NewSelect(labels, func(value string) {
			item.Selected = ids[value]
			showChoice(item.Selected)
		})
		if len(labels) > 0 {
			for _, c := range item.Choices {
				if c.ID == item.Selected {
					selectFormat.SetSelected(c.Label)
					showChoice(c.ID)
					break
				}
			}
		}
		content := container.NewVBox(widget.NewLabel("原始链接："+item.Source), container.NewHBox(widget.NewLabel("清晰度"), selectFormat), supportNote, widget.NewLabel("视频流地址（可复制）："), streamEntry)
		objects = append(objects, widget.NewCard(title, "解析结果", content))
	}
	a.parsedList.Objects = objects
	a.parsedList.Refresh()
}

func (a *App) addParsedTasks() {
	if len(a.parseItems) == 0 {
		dialog.ShowInformation("没有解析结果", "请先解析链接。", a.window)
		return
	}
	readyCount := 0
	for _, item := range a.parseItems {
		if item.Err != "" || item.Selected == "" {
			continue
		}
		for _, choice := range item.Choices {
			if choice.ID == item.Selected {
				readyCount++
				break
			}
		}
	}
	if readyCount == 0 {
		dialog.ShowInformation("没有可下载任务", "请选择一个已解析的清晰度后再加入队列。", a.window)
		return
	}
	outDir := strings.TrimSpace(a.outputEntry.Text)
	if outDir == "" {
		outDir = a.state.OutputDir
	}
	if err := os.MkdirAll(outDir, 0755); err != nil {
		dialog.ShowError(err, a.window)
		return
	}
	maxHeight := a.state.MaxHeight
	a.mu.Lock()
	for _, item := range a.parseItems {
		if item.Err != "" || item.Selected == "" {
			continue
		}
		var selectedChoice *tools.FormatChoice
		for _, choice := range item.Choices {
			if choice.ID == item.Selected {
				selectedChoice = &choice
				break
			}
		}
		if selectedChoice == nil {
			continue
		}
		name := safeName(item.Title)
		if name == "" {
			name = fmt.Sprintf("video_%d", time.Now().UnixNano())
		}
		choiceName := item.Selected
		for _, c := range item.Choices {
			if c.ID == item.Selected {
				choiceName = c.Label
				break
			}
		}
		outputPath := uniqueOutputPath(outDir, name+".mp4", a.state.Tasks)
		t := task{ID: fmt.Sprintf("%d_%d", time.Now().UnixNano(), len(a.state.Tasks)), Source: item.Source, Title: item.Title, FormatID: item.Selected, FormatName: choiceName, UseYTDLP: !selectedChoice.Supported, YTDLPFormat: selectedChoice.YTDLPFormat, MaxHeight: maxHeight, Output: outputPath, Status: "等待中", UpdatedAt: time.Now()}
		a.state.Tasks = append(a.state.Tasks, t)
	}
	a.state.OutputDir = outDir
	a.saveLocked()
	a.mu.Unlock()
	a.parseStatus.SetText(fmt.Sprintf("已加入 %d 条，队列已开始下载。", readyCount))
	a.refreshTasks()
	a.startQueue()
}

func (a *App) startQueue() {
	a.mu.Lock()
	if a.workerRunning {
		a.mu.Unlock()
		return
	}
	a.workerRunning = true
	a.mu.Unlock()
	go func() {
		for {
			a.mu.Lock()
			index := -1
			for i := range a.state.Tasks {
				if a.state.Tasks[i].Status == "等待中" {
					index = i
					break
				}
			}
			if index < 0 {
				a.workerRunning = false
				a.mu.Unlock()
				return
			}
			t := a.state.Tasks[index]
			ctx, cancel := context.WithCancel(context.Background())
			a.activeID = t.ID
			a.activeCancel = cancel
			a.state.Tasks[index].Status = "下载中"
			a.state.Tasks[index].UpdatedAt = time.Now()
			a.saveLocked()
			a.mu.Unlock()
			a.refreshTasks()
			err := a.downloadTask(ctx, t)
			cancel()
			a.mu.Lock()
			for i := range a.state.Tasks {
				if a.state.Tasks[i].ID != t.ID {
					continue
				}
				if a.state.Tasks[i].Status == "暂停中" || a.state.Tasks[i].Status == "已取消" || (a.state.Tasks[i].Status == "等待中" && errors.Is(err, context.Canceled)) {
				} else if err != nil {
					a.state.Tasks[i].Status = "失败"
					a.state.Tasks[i].Error = err.Error()
				} else {
					a.state.Tasks[i].Status = "已完成"
					a.state.Tasks[i].Done = a.state.Tasks[i].Total
				}
				a.state.Tasks[i].UpdatedAt = time.Now()
				break
			}
			a.activeID = ""
			a.activeCancel = nil
			a.saveLocked()
			a.mu.Unlock()
			a.refreshTasks()
		}
	}()
}

func (a *App) downloadTask(ctx context.Context, t task) error {
	a.mu.Lock()
	toolSet := a.toolSet
	proxyURL := a.state.ProxyURL
	a.mu.Unlock()
	toolSet.YTDLP.Proxy = proxyURL
	if t.UseYTDLP {
		lastPersist := time.Now()
		return tools.DownloadWithYTDLP(ctx, toolSet.YTDLP, t.Source, t.YTDLPFormat, t.Output, toolSet.FFmpeg, t.MaxHeight, func(done, total int64) {
			a.mu.Lock()
			for i := range a.state.Tasks {
				if a.state.Tasks[i].ID == t.ID {
					a.state.Tasks[i].Done = int(done)
					a.state.Tasks[i].Total = int(total)
					a.state.Tasks[i].UpdatedAt = time.Now()
					break
				}
			}
			if done >= total || time.Since(lastPersist) >= time.Second {
				a.saveLocked()
				lastPersist = time.Now()
			}
			a.mu.Unlock()
			a.refreshTasks()
		})
	}
	stream, err := tools.ResolveHLSFormat(ctx, toolSet.YTDLP, t.Source, t.FormatID, t.MaxHeight)
	if err != nil {
		return err
	}
	if stream.URL == "" {
		stream.URL = t.Source
	}
	lastPersist := time.Now()
	return hls.Download(ctx, hls.Options{URL: stream.URL, Output: t.Output, WorkDir: t.Output + ".work", Workers: 8, Retries: 5, MaxHeight: t.MaxHeight, FFmpegPath: toolSet.FFmpeg, Proxy: proxyURL, Progress: func(p hls.Progress) {
		a.mu.Lock()
		for i := range a.state.Tasks {
			if a.state.Tasks[i].ID == t.ID {
				a.state.Tasks[i].Done = p.Done
				a.state.Tasks[i].Total = p.Total
				a.state.Tasks[i].UpdatedAt = time.Now()
				break
			}
		}
		if p.Done >= p.Total || time.Since(lastPersist) >= time.Second {
			a.saveLocked()
			lastPersist = time.Now()
		}
		a.mu.Unlock()
		a.refreshTasks()
	}})
}

func (a *App) pauseTask(id string) {
	a.mu.Lock()
	for i := range a.state.Tasks {
		if a.state.Tasks[i].ID == id && a.state.Tasks[i].Status == "下载中" {
			a.state.Tasks[i].Status = "暂停中"
			a.state.Tasks[i].UpdatedAt = time.Now()
			if a.activeID == id && a.activeCancel != nil {
				a.activeCancel()
			}
			break
		}
	}
	a.saveLocked()
	a.mu.Unlock()
	a.refreshTasks()
}

func (a *App) resumeTask(id string) {
	a.mu.Lock()
	for i := range a.state.Tasks {
		if a.state.Tasks[i].ID == id && a.state.Tasks[i].Status == "暂停中" {
			a.state.Tasks[i].Status = "等待中"
			a.state.Tasks[i].Error = ""
			a.state.Tasks[i].UpdatedAt = time.Now()
			break
		}
	}
	a.saveLocked()
	a.mu.Unlock()
	a.refreshTasks()
	a.startQueue()
}

func (a *App) cancelTask(id string) {
	a.mu.Lock()
	for i := range a.state.Tasks {
		if a.state.Tasks[i].ID == id && (a.state.Tasks[i].Status == "等待中" || a.state.Tasks[i].Status == "暂停中" || a.state.Tasks[i].Status == "下载中") {
			a.state.Tasks[i].Status = "已取消"
			a.state.Tasks[i].UpdatedAt = time.Now()
			if a.activeID == id && a.activeCancel != nil {
				a.activeCancel()
			}
			break
		}
	}
	a.saveLocked()
	a.mu.Unlock()
	a.refreshTasks()
}

func isHistoryTask(status string) bool {
	return status == "已完成" || status == "失败" || status == "已取消"
}

func (a *App) deleteHistoryTask(id string) {
	a.mu.Lock()
	filtered := a.state.Tasks[:0]
	for _, t := range a.state.Tasks {
		if t.ID == id && isHistoryTask(t.Status) {
			continue
		}
		filtered = append(filtered, t)
	}
	a.state.Tasks = filtered
	a.saveLocked()
	a.mu.Unlock()
	a.refreshTasks()
}

func (a *App) clearHistory() {
	a.mu.Lock()
	filtered := a.state.Tasks[:0]
	for _, t := range a.state.Tasks {
		if !isHistoryTask(t.Status) {
			filtered = append(filtered, t)
		}
	}
	a.state.Tasks = filtered
	a.saveLocked()
	a.mu.Unlock()
	a.refreshTasks()
}

func (a *App) refreshTasks() {
	if a.taskList == nil {
		return
	}
	a.mu.Lock()
	tasks := append([]task(nil), a.state.Tasks...)
	a.mu.Unlock()
	fyne.Do(func() {
		active := make([]fyne.CanvasObject, 0)
		history := make([]fyne.CanvasObject, 0)
		for _, t := range tasks {
			status := t.Status
			if isHistoryTask(status) {
				history = append(history, a.taskCard(t, false))
				continue
			}
			active = append(active, a.taskCard(t, true))
		}
		if len(active) == 0 {
			active = append(active, widget.NewLabel("队列为空。解析链接后可批量加入任务。"))
		}
		a.taskList.Objects = active
		a.taskList.Refresh()
		if a.historyList != nil {
			if len(history) == 0 {
				history = append(history, widget.NewLabel("还没有历史任务。"))
			}
			a.historyList.Objects = history
			a.historyList.Refresh()
		}
	})
}

func (a *App) taskCard(t task, allowActions bool) fyne.CanvasObject {
	title := t.Title
	if title == "" {
		title = t.Source
	}
	info := fmt.Sprintf("%s  ·  %s", t.FormatName, t.Status)
	if t.UseYTDLP {
		info += " · yt-dlp"
	}
	progress := widget.NewProgressBar()
	if t.Total > 0 {
		progress.SetValue(float64(t.Done) / float64(t.Total))
	} else if t.Status == "下载中" {
		progress.SetValue(0)
	}
	detail := widget.NewLabel(t.Error)
	if t.Total > 0 {
		if t.UseYTDLP {
			detail.SetText(fmt.Sprintf("已下载 %s / %s\n%s", formatBytes(int64(t.Done)), formatBytes(int64(t.Total)), t.Output))
		} else {
			detail.SetText(fmt.Sprintf("分片 %d / %d\n%s", t.Done, t.Total, t.Output))
		}
	} else if t.Error == "" {
		detail.SetText(t.Output)
	}
	items := []fyne.CanvasObject{progress, detail}
	if allowActions {
		actions := container.NewHBox()
		if t.Status == "下载中" {
			actions.Add(widget.NewButton("暂停", func() { a.pauseTask(t.ID) }))
		}
		if t.Status == "暂停中" {
			actions.Add(widget.NewButton("继续", func() { a.resumeTask(t.ID) }))
		}
		if t.Status == "等待中" || t.Status == "下载中" || t.Status == "暂停中" {
			actions.Add(widget.NewButton("取消", func() { a.cancelTask(t.ID) }))
		}
		actions.Add(layout.NewSpacer())
		actions.Add(widget.NewButton("打开目录", func() { _ = exec.Command("explorer.exe", filepath.Dir(t.Output)).Start() }))
		items = append(items, actions)
	} else {
		items = append(items, container.NewHBox(layout.NewSpacer(), widget.NewButton("删除记录", func() {
			a.deleteHistoryTask(t.ID)
		})))
	}
	return widget.NewCard(title, info, container.NewVBox(items...))
}

func (a *App) loadState() {
	b, err := os.ReadFile(a.statePath)
	if err != nil {
		return
	}
	var state appState
	if json.Unmarshal(b, &state) != nil {
		return
	}
	if state.OutputDir != "" {
		a.state.OutputDir = state.OutputDir
	}
	a.state.MaxHeight = state.MaxHeight
	a.state.YTDLPPath = state.YTDLPPath
	a.state.FFmpegPath = state.FFmpegPath
	a.state.ProxyURL = state.ProxyURL
	for i := range state.Tasks {
		if state.Tasks[i].Status == "下载中" {
			state.Tasks[i].Status = "暂停中"
		}
	}
	a.state.Tasks = state.Tasks
}

func (a *App) saveState() { a.mu.Lock(); a.saveLocked(); a.mu.Unlock() }
func (a *App) saveLocked() {
	a.state.Tasks = append([]task(nil), a.state.Tasks...)
	b, err := json.MarshalIndent(a.state, "", "  ")
	if err != nil {
		return
	}
	tmp := a.statePath + ".tmp"
	if os.WriteFile(tmp, b, 0600) != nil {
		return
	}
	_ = os.Remove(a.statePath)
	_ = os.Rename(tmp, a.statePath)
}

func splitURLs(raw string) []string {
	lines := strings.FieldsFunc(raw, func(r rune) bool { return r == '\n' || r == '\r' || r == '\t' || r == ' ' })
	seen := map[string]bool{}
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" && !seen[line] {
			seen[line] = true
			out = append(out, line)
		}
	}
	return out
}
func parseHeight(label string) int { var n int; _, _ = fmt.Sscanf(label, "%dp", &n); return n }
func heightLabel(h int) string {
	if h <= 0 {
		return "自动最高"
	}
	return fmt.Sprintf("%dp", h)
}
func safeName(s string) string {
	s = strings.TrimSpace(s)
	replacer := strings.NewReplacer("<", "_", ">", "_", ":", "_", "\"", "_", "/", "_", "\\", "_", "|", "_", "?", "_", "*", "_")
	s = replacer.Replace(s)
	s = strings.Trim(s, " .")
	if runes := []rune(s); len(runes) > 120 {
		s = string(runes[:120])
	}
	return s
}

func uniqueOutputPath(dir, filename string, tasks []task) string {
	ext := filepath.Ext(filename)
	stem := strings.TrimSuffix(filename, ext)
	used := make(map[string]struct{}, len(tasks))
	for _, t := range tasks {
		used[strings.ToLower(filepath.Clean(t.Output))] = struct{}{}
	}
	for suffix := 0; ; suffix++ {
		candidateName := filename
		if suffix > 0 {
			candidateName = fmt.Sprintf("%s (%d)%s", stem, suffix, ext)
		}
		candidate := filepath.Join(dir, candidateName)
		if _, exists := used[strings.ToLower(filepath.Clean(candidate))]; exists {
			continue
		}
		if _, err := os.Stat(candidate); errors.Is(err, os.ErrNotExist) {
			return candidate
		} else if err != nil {
			// The downloader will surface an access error; it also refuses overwrites.
			return candidate
		}
	}
}
