//go:build windows
// +build windows

package main

import (
	"syscall"
	"time"
	"unsafe"

	"github.com/jchv/go-webview2"
)

// runDesktopGUI 在 Windows 下启动原生桌面窗口 (WebView2)。
// 当用户关闭窗口时，返回以继续退出流程。若环境缺少 WebView2 运行时或初始化异常，安全降级回退到系统浏览器。
func runDesktopGUI(uiURL string, stopFunc func()) (handled bool) {
	defer func() {
		if r := recover(); r != nil {
			// 若 WebView2 初始化或运行过程崩溃，安全降级
			handled = false
		}
	}()

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

// showNativeError 在 GUI 模式发生致命错误时弹出 Windows 原生弹窗
func showNativeError(title, msg string) {
	user32 := syscall.NewLazyDLL("user32.dll")
	procMessageBoxW := user32.NewProc("MessageBoxW")
	tPtr, _ := syscall.UTF16PtrFromString(title)
	mPtr, _ := syscall.UTF16PtrFromString(msg)
	// 0x10 = MB_ICONERROR | MB_OK
	procMessageBoxW.Call(0, uintptr(unsafe.Pointer(mPtr)), uintptr(unsafe.Pointer(tPtr)), 0x10)
}

