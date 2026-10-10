//go:build windows

package sysproc

import (
	"os/exec"
	"syscall"
)

// HideWindow 在 Windows 平台为子进程配置静默无窗口属性（CREATE_NO_WINDOW 与 HideWindow），
// 彻底杜绝在 GUI/后台运行时频繁闪现黑色 CMD/控制台黑框窗口。
func HideWindow(cmd *exec.Cmd) *exec.Cmd {
	if cmd == nil {
		return nil
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= 0x08000000 // CREATE_NO_WINDOW
	return cmd
}
