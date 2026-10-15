//go:build windows

// 深色模式
//
// 纯 Win32 没有「一键深色主题」，需要自己处理这几件事：
//   1. 标题栏        —— DwmSetWindowAttribute(DWMWA_USE_IMMERSIVE_DARK_MODE)
//   2. 窗口背景      —— 拦截 WM_ERASEBKGND 自绘
//   3. 静态框/编辑框 —— 拦截 WM_CTLCOLOR*，返回深色画刷 + 浅色文字
//   4. 列表控件      —— SetWindowTheme("DarkMode_Explorer") + 三项颜色消息
//   5. 按钮/复选框   —— BS_OWNERDRAW 自绘（系统按钮不跟随主题，只能自己画）
package main

import (
	"runtime"
	"syscall"
	"unsafe"
)

var (
	dwmapi   = syscall.NewLazyDLL("dwmapi.dll")
	uxtheme  = syscall.NewLazyDLL("uxtheme.dll")
	gdi32d2  = syscall.NewLazyDLL("gdi32.dll")

	pDwmSetWindowAttr = dwmapi.NewProc("DwmSetWindowAttribute")
	pSetWindowTheme   = uxtheme.NewProc("SetWindowTheme")

	pCreateSolidBrush = gdi32d2.NewProc("CreateSolidBrush")
	pDeleteObject     = gdi32d2.NewProc("DeleteObject")
	pSetTextColor     = gdi32d2.NewProc("SetTextColor")
	pSetBkColor       = gdi32d2.NewProc("SetBkColor")
	pSetBkMode        = gdi32d2.NewProc("SetBkMode")

	pFillRect      = user32.NewProc("FillRect")
	pFrameRect     = user32.NewProc("FrameRect")
	pDrawTextW     = user32.NewProc("DrawTextW")
	pGetClientRect = user32.NewProc("GetClientRect")
)

// 控件颜色消息
const (
	wmCtlColorBtn     = 0x0135
	wmCtlColorEdit    = 0x0133
	wmCtlColorListBox = 0x0134
	wmCtlColorStatic  = 0x0138
)

func rgb(r, g, b byte) uintptr {
	return uintptr(r) | uintptr(g)<<8 | uintptr(b)<<16
}

// 调色板
var (
	dBg      = rgb(32, 32, 32)    // 窗口背景
	dEditBg  = rgb(45, 45, 48)    // 输入框 / 日志背景
	dText    = rgb(228, 228, 228) // 正文
	dTextDim = rgb(150, 150, 150) // 禁用文字
	dBtnBg   = rgb(62, 62, 66)    // 按钮常态
	dBtnDown = rgb(42, 42, 46)    // 按钮按下
	dBorder  = rgb(96, 96, 102)   // 边框

	// 浅色（自绘按钮在浅色下走同一条绘制路径，保证外观一致）
	lBtnBg   = rgb(240, 240, 240)
	lBtnDown = rgb(202, 202, 202)
	lBorder  = rgb(173, 173, 173)
	lText    = rgb(0, 0, 0)
	lTextDim = rgb(150, 150, 150)
)

var (
	brDarkBg, brEditBg, brBtnBg, brBtnDown uintptr
	brLightBg, brLightDown                 uintptr
	brBorderDark, brBorderLight            uintptr
)

// darkMode 当前是否深色
var darkMode bool

func initDarkResources() {
	if brDarkBg != 0 {
		return
	}
	mk := func(c uintptr) uintptr {
		b, _, _ := pCreateSolidBrush.Call(c)
		return b
	}
	brDarkBg = mk(dBg)
	brEditBg = mk(dEditBg)
	brBtnBg = mk(dBtnBg)
	brBtnDown = mk(dBtnDown)
	brLightBg = mk(lBtnBg)
	brLightDown = mk(lBtnDown)
	brBorderDark = mk(dBorder)
	brBorderLight = mk(lBorder)
}

// ---- WM_DRAWITEM：自绘按钮与复选框 ----

type drawItemStruct struct {
	ctlType    uint32
	ctlID      uint32
	itemID     uint32
	itemAction uint32
	itemState  uint32
	hwndItem   syscall.Handle
	hDC        syscall.Handle
	rcItem     rect
	itemData   uintptr
}

const (
	odsSelected = 0x0001
	odsDisabled = 0x0004

	dtCenter      = 0x00000001
	dtVCenter     = 0x00000004
	dtSingleLine  = 0x00000020
	dtEndEllipsis = 0x00004000

	bmGetCheck   = 0x00F0
	bmSetCheck   = 0x00F1
	bmGetState   = 0x00F2
	bstChecked   = 0x0001
	bstUnchecked = 0x0000
)

// drawOwnerItem 处理 WM_DRAWITEM，返回 1 表示已处理
func drawOwnerItem(l unsafe.Pointer) uintptr {
	if l == nil {
		return 0
	}
	dis := (*drawItemStruct)(l)
	initDarkResources()

	// 复选框与普通按钮共用绘制，只在文字前加一个勾选符号
	isCheck := int(dis.ctlID) == idShow || int(dis.ctlID) == idDark

	bg, fg, bd := brBtnBg, dText, brBorderDark
	if !darkMode {
		bg, fg, bd = brLightBg, lText, brBorderLight
	}
	if dis.itemState&odsDisabled != 0 {
		if darkMode {
			fg = dTextDim
		} else {
			fg = lTextDim
		}
	} else if dis.itemState&odsSelected != 0 {
		if darkMode {
			bg = brBtnDown
		} else {
			bg = brLightDown
		}
	}

	r := dis.rcItem
	pFillRect.Call(uintptr(dis.hDC), uintptr(unsafe.Pointer(&r)), bg)
	pFrameRect.Call(uintptr(dis.hDC), uintptr(unsafe.Pointer(&r)), bd)

	label := getText(dis.hwndItem)
	if isCheck {
		st, _, _ := pSendMessageW.Call(uintptr(dis.hwndItem), bmGetCheck, 0, 0)
		if st == bstChecked {
			label = "☑ " + label
		} else {
			label = "☐ " + label
		}
	}

	pSetBkMode.Call(uintptr(dis.hDC), 1 /*TRANSPARENT*/)
	pSetTextColor.Call(uintptr(dis.hDC), fg)
	txt := u16(label)
	pDrawTextW.Call(uintptr(dis.hDC), uintptr(unsafe.Pointer(txt)), ^uintptr(0),
		uintptr(unsafe.Pointer(&r)), dtCenter|dtVCenter|dtSingleLine|dtEndEllipsis)
	runtime.KeepAlive(txt)
	return 1
}

// ---- WM_CTLCOLOR*：静态框 / 编辑框配色 ----

func ctlColor(msg uint32, hdc syscall.Handle, ctl unsafe.Pointer) uintptr {
	if !darkMode {
		return 0
	}
	initDarkResources()

	switch msg {
	case wmCtlColorStatic:
		// 只读的多行编辑框（日志）也走 STATIC 分支
		if uintptr(ctl) == uintptr(hLog) {
			pSetTextColor.Call(uintptr(hdc), dText)
			pSetBkColor.Call(uintptr(hdc), dEditBg)
			return brEditBg
		}
		pSetTextColor.Call(uintptr(hdc), dText)
		pSetBkMode.Call(uintptr(hdc), 1)
		return brDarkBg
	case wmCtlColorEdit, wmCtlColorListBox:
		pSetTextColor.Call(uintptr(hdc), dText)
		pSetBkColor.Call(uintptr(hdc), dEditBg)
		return brEditBg
	case wmCtlColorBtn:
		// 分组框文字
		pSetTextColor.Call(uintptr(hdc), dText)
		pSetBkMode.Call(uintptr(hdc), 1)
		return brDarkBg
	}
	return 0
}

// ---- 应用 / 取消深色 ----

func applyDarkMode(on bool) {
	darkMode = on
	initDarkResources()

	// 1) 标题栏：DWMWA_USE_IMMERSIVE_DARK_MODE
	//    Win10 1809+ 是 20，更早的预览版是 19，两个都发一次，失败无所谓
	use := uintptr(0)
	if on {
		use = 1
	}
	pDwmSetWindowAttr.Call(uintptr(hMain), 20, uintptr(unsafe.Pointer(&use)), 4)
	pDwmSetWindowAttr.Call(uintptr(hMain), 19, uintptr(unsafe.Pointer(&use)), 4)

	// 2) 列表控件：切换主题 + 三项颜色
	if hList != 0 {
		theme := "DarkMode_Explorer"
		if !on {
			theme = "Explorer"
		}
		th := u16(theme)
		pSetWindowTheme.Call(uintptr(hList), uintptr(unsafe.Pointer(th)), 0)
		runtime.KeepAlive(th)

		bk, tx := uintptr(rgb(255, 255, 255)), uintptr(rgb(0, 0, 0))
		if on {
			bk, tx = dEditBg, dText
		}
		pSendMessageW.Call(uintptr(hList), 0x1001 /*LVM_SETBKCOLOR*/, 0, bk)
		pSendMessageW.Call(uintptr(hList), 0x1024 /*LVM_SETTEXTCOLOR*/, 0, tx)
		pSendMessageW.Call(uintptr(hList), 0x1026 /*LVM_SETTEXTBKCOLOR*/, 0, bk)
	}

	// 3) 日志框自身背景
	if hLog != 0 {
		if on {
			pSendMessageW.Call(uintptr(hLog), 0x0443 /*EM_SETBKGNDCOLOR*/, 0, dEditBg)
		} else {
			pSendMessageW.Call(uintptr(hLog), 0x0443, 1, 0) // 恢复系统默认
		}
	}

	// 4) 重绘
	pInvalidateRect.Call(uintptr(hMain), 0, 1)
	pUpdateWindow.Call(uintptr(hMain))
}

// eraseBackground 处理 WM_ERASEBKGND
func eraseBackground(hwnd syscall.Handle, hdc syscall.Handle) uintptr {
	if !darkMode {
		return 0
	}
	initDarkResources()
	var r rect
	pGetClientRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&r)))
	pFillRect.Call(uintptr(hdc), uintptr(unsafe.Pointer(&r)), brDarkBg)
	return 1
}

// ---- 复选框状态 ----

// 自绘按钮不参与系统自动勾选，状态由我们自己维护
var (
	hShowChk syscall.Handle
	hDarkChk syscall.Handle
)

func setCheck(h syscall.Handle, on bool) {
	if h == 0 {
		return
	}
	v := uintptr(bstUnchecked)
	if on {
		v = bstChecked
	}
	pSendMessageW.Call(uintptr(h), bmSetCheck, v, 0)
	pInvalidateRect.Call(uintptr(h), 0, 1)
}

// onToggleDark 处理「深色」勾选
func onToggleDark() {
	on := !darkMode
	applyDarkMode(on)
	setCheck(hDarkChk, on)
	if c := collectQuiet(); c != nil {
		saveCfg(c)
	}
	if on {
		log("[主题] 已切换到深色模式")
	} else {
		log("[主题] 已切换到浅色模式")
	}
}

