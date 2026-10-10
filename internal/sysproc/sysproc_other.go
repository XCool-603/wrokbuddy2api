//go:build !windows

package sysproc

import "os/exec"

// HideWindow 在非 Windows 平台无须设置窗口属性，直接返回命令本身。
func HideWindow(cmd *exec.Cmd) *exec.Cmd {
	return cmd
}
