//go:build !windows
// +build !windows

package main

// runDesktopGUI 在非 Windows 系统（Linux/macOS）下的打桩实现。
// 非 Windows 下不启动 webview2，返回 false 以回退到无头模式或浏览器打开。
func runDesktopGUI(uiURL string, stopFunc func()) bool {
	return false
}
