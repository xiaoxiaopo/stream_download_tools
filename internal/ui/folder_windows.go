//go:build windows

package ui

import (
	"fmt"
	"os/exec"
	"strings"
	"syscall"
)

func pickNativeFolder(title string) (string, error) {
	title = strings.ReplaceAll(title, "'", "''")
	script := "Add-Type -AssemblyName System.Windows.Forms; " +
		"[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false); " +
		"[System.Windows.Forms.Application]::EnableVisualStyles(); " +
		"$picker = New-Object System.Windows.Forms.FolderBrowserDialog; " +
		"$picker.Description = '" + title + "'; " +
		"$picker.ShowNewFolderButton = $true; " +
		"if ($picker.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) { [Console]::Out.Write($picker.SelectedPath) }"
	cmd := exec.Command("powershell.exe", "-NoProfile", "-STA", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("打开 Windows 文件夹选择器失败：%w", err)
	}
	return strings.TrimRight(string(output), "\r\n"), nil
}
