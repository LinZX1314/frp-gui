//go:build windows

// 现代文件选择器：IFileOpenDialog（Vista+ 官方推荐替代 GetOpenFileName）
//
// 背景：GetOpenFileNameW 是已弃用的老 API，在 Windows 11 24H2 上实测必定
// 返回 CDERR_INITIALIZATION(2) 打不开对话框。已排除的原因包括：
//   - 结构体布局错误（逐字段校验：size=136，偏移与 Win32 一致）
//   - 过滤器串为 nil（syscall.UTF16FromString 遇内嵌 NUL 返回 EINVAL，已修）
//   - COM 未初始化（OleInitialize 返回 S_OK）
//   - 公共控件未加载（已补全 ICC_WIN95_CLASSES|ICC_STANDARD_CLASSES）
//   - 缺 comctl32 v6 manifest（已内嵌 rsrc.syso，仍无效）
// 微软文档明确建议改用 Common Item Dialog，本文件即该 API 的纯 COM 实现。
package main

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

var (
	ole32dl = syscall.NewLazyDLL("ole32.dll")

	pCoInitializeEx   = ole32dl.NewProc("CoInitializeEx")
	pCoCreateInstance = ole32dl.NewProc("CoCreateInstance")
	pCoTaskMemFree    = ole32dl.NewProc("CoTaskMemFree")
)

// guid 必须与 Win32 内存布局一致（16 字节）
type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

var (
	clsidFileOpenDialog = guid{0xDC1C5A9C, 0xE88A, 0x4DDE,
		[8]byte{0xA5, 0xA1, 0x60, 0xF8, 0x2A, 0x20, 0xAE, 0xF7}}
	iidIFileOpenDialog = guid{0xD57C7288, 0xD4AD, 0x4768,
		[8]byte{0xBE, 0x02, 0x9D, 0x96, 0x95, 0x32, 0xD9, 0x60}}
)

const (
	coinitApartmentThreaded = 0x2
	rpcEChangedMode         = 0x80010106 // 已经初始化过，可继续用
	clsctxInprocServer      = 0x1

	fosNoChangeDir     = 0x00000008
	fosPathMustExist   = 0x00000800
	fosFileMustExist   = 0x00001000
	fosForceFileSystem = 0x00000040
	fosDontAddToRecent = 0x02000000

	sigdnFileSysPath = 0x80058000
	hrCancelled      = 0x800704C7

	// COM 虚表索引（前 3 个是 IUnknown）
	vtblRelease   = 2
	vtblShow      = 3  // IModalWindow::Show
	vtblSetOpts   = 9  // IFileDialog::SetOptions
	vtblSetTitle  = 17 // IFileDialog::SetTitle
	vtblGetResult = 20 // IFileDialog::GetResult
	vtblGetDispNm = 5  // IShellItem::GetDisplayName
)

// vtblCall 调用 COM 接口虚函数；this 必须是第一个参数。
// 用 unsafe.Pointer 而非 uintptr 承载接口指针，避免 vet 的指针误用告警。
func vtblCall(this unsafe.Pointer, idx int, args ...uintptr) uintptr {
	if this == nil {
		return 0x80004003 // E_POINTER
	}
	vtbl := *(*unsafe.Pointer)(this)
	fn := *(*uintptr)(unsafe.Add(vtbl, uintptr(idx)*unsafe.Sizeof(uintptr(0))))
	all := append([]uintptr{uintptr(this)}, args...)
	r, _, _ := syscall.SyscallN(fn, all...)
	return r
}

// pickExeFileModern 用 IFileOpenDialog 弹出系统文件选择框。
// 返回 ("路径", nil) 选好；("", nil) 用户取消；("", err) 失败。
func pickExeFileModern(owner syscall.Handle, title string) (string, error) {
	// S_FALSE(1) 与 RPC_E_CHANGED_MODE 都表示 COM 已可用
	if hr, _, _ := pCoInitializeEx.Call(0, coinitApartmentThreaded); int32(hr) < 0 &&
		uint32(hr) != rpcEChangedMode {
		return "", fmt.Errorf("COM 初始化失败：0x%08X", uint32(hr))
	}

	var pDialog unsafe.Pointer
	hr, _, _ := pCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&clsidFileOpenDialog)),
		0,
		clsctxInprocServer,
		uintptr(unsafe.Pointer(&iidIFileOpenDialog)),
		uintptr(unsafe.Pointer(&pDialog)),
	)
	if int32(hr) < 0 || pDialog == nil {
		return "", fmt.Errorf("无法创建文件对话框：0x%08X", uint32(hr))
	}
	defer vtblCall(pDialog, vtblRelease)

	vtblCall(pDialog, vtblSetOpts, uintptr(
		fosForceFileSystem|fosFileMustExist|fosPathMustExist|
			fosNoChangeDir|fosDontAddToRecent))

	if title != "" {
		// 显式持有 UTF-16 缓冲，保证调用期间不被 GC 回收
		tp, err := syscall.UTF16PtrFromString(title)
		if err == nil {
			vtblCall(pDialog, vtblSetTitle, uintptr(unsafe.Pointer(tp)))
			runtime.KeepAlive(tp)
		}
	}

	hr = vtblCall(pDialog, vtblShow, uintptr(owner))
	if int32(hr) < 0 {
		if uint32(hr) == hrCancelled {
			return "", nil // 用户取消
		}
		return "", fmt.Errorf("打开文件对话框失败：0x%08X", uint32(hr))
	}

	var pItem unsafe.Pointer
	if hr = vtblCall(pDialog, vtblGetResult,
		uintptr(unsafe.Pointer(&pItem))); int32(hr) < 0 || pItem == nil {
		return "", fmt.Errorf("无法取得选择结果：0x%08X", uint32(hr))
	}
	defer vtblCall(pItem, vtblRelease)

	var pPath unsafe.Pointer
	if hr = vtblCall(pItem, vtblGetDispNm, uintptr(sigdnFileSysPath),
		uintptr(unsafe.Pointer(&pPath))); int32(hr) < 0 || pPath == nil {
		return "", fmt.Errorf("无法取得文件路径：0x%08X", uint32(hr))
	}
	defer pCoTaskMemFree.Call(uintptr(pPath))

	// 读 UTF-16 直到 NUL；32768 是安全上限（路径远短于此）
	buf := unsafe.Slice((*uint16)(pPath), 32768)
	return syscall.UTF16ToString(buf), nil
}
