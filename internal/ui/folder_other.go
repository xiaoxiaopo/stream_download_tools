//go:build !windows

package ui

import "errors"

func pickNativeFolder(string) (string, error) {
	return "", errors.New("原生文件夹选择器目前仅支持 Windows")
}
