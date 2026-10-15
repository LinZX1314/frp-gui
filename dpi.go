//go:build windows

// 高 DPI 支持
//
// 两个动作必须一起做，缺一个都会出问题：
//   1. SetProcessDPIAware()      —— 不声明感知时 Windows 会把整个窗口位图放大，字发虚
//   2. 按系统 DPI 缩放所有坐标   —— 只声明感知而不缩放，界面在高 DPI 屏上会变小
//
// 本程序所有尺寸都是 96 DPI 下的「设计像素」，统一经由 ds() 换算。
package main

import (
	"syscall"
)

var (
	user32d = syscall.NewLazyDLL("user32.dll")
	gdi32d  = syscall.NewLazyDLL("gdi32.dll")

	pSetProcessDPIAware = user32d.NewProc("SetProcessDPIAware")
	pGetDC              = user32d.NewProc("GetDC")
	pReleaseDC          = user32d.NewProc("ReleaseDC")
	pGetDeviceCaps      = gdi32d.NewProc("GetDeviceCaps")
)

const logPixelsX = 88 // GetDeviceCaps 的 LOGPIXELSX

// dpiScale：1.0 = 96 DPI（即 100% 显示缩放）
var dpiScale = 1.0

// ds 把设计像素换算成当前 DPI 下的实际像素
func ds(v int) int {
	if dpiScale == 1.0 {
		return v
	}
	return int(float64(v)*dpiScale + 0.5)
}

// initDPI 声明进程 DPI 感知并计算缩放系数。必须在创建窗口/字体之前调用。
func initDPI() {
	pSetProcessDPIAware.Call()

	hdc, _, _ := pGetDC.Call(0)
	if hdc == 0 {
		return
	}
	dpi, _, _ := pGetDeviceCaps.Call(hdc, logPixelsX)
	pReleaseDC.Call(0, hdc)

	// 合理范围校验，避免异常 DPI 把界面撑爆
	if dpi >= 48 && dpi <= 480 {
		dpiScale = float64(dpi) / 96.0
	}
}
