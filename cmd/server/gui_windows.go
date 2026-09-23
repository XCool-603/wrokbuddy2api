//go:build windows
// +build windows

package main

import (
	"time"

	"github.com/jchv/go-webview2"
)

// runDesktopGUI 在 Windows 下启动原生桌面窗口 (WebView2)。
// 当用户关闭窗口时，返回以继续退出流程。
func runDesktopGUI(uiURL string, stopFunc func()) bool {
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug: false,
		WindowOptions: webview2.WindowOptions{
			Title:  "WorkBuddy 2API",
			Width:  1280,
			Height: 820,
			IconId: 2,
			Center: true,
		},
	})
	if w != nil {
		defer w.Destroy()
		w.Navigate(uiURL)
		w.Run()
		if stopFunc != nil {
			stopFunc()
		}
		time.Sleep(200 * time.Millisecond)
		return true
	}
	return false
}
