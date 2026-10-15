//go:build windows

// frpc-gui: FRP 客户端图形界面启动器
//
// 设计：本程序【不携带、不释放】frpc.exe，只负责生成配置并调用用户指定的 frpc.exe。
// 这样可避免杀软把「内嵌 PE + 运行时释放执行」误判成木马释放器。
//
// 构建: go build -ldflags "-H windowsgui -s -w" -o frpc-gui.exe
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const (
	appName = "FRP 客户端启动器"

	wsOverlapped = 0x00000000
	wsCaption    = 0x00C00000
	wsSysMenu    = 0x00080000
	wsVisible    = 0x10000000
	wsChild      = 0x40000000
	wsBorder     = 0x00800000
	wsTabStop    = 0x00010000
	wsVScroll    = 0x00200000
	wsMinimize   = 0x20000000

	bsGroupBox   = 0x00000007
	bsPushButton = 0x00000000
	bsAutoCheck  = 0x00000003

	esLeft        = 0x0000
	esAutoHScroll = 0x0080
	esReadOnly    = 0x0800
	esMultiline   = 0x0004
	esPassword    = 0x0020

	swShow = 5

	emSetSel     = 0x00B1
	emReplaceSel = 0x00C2
	emSetPwdChar = 0x00CC
	wmClose      = 0x0010
	wmDestroy    = 0x0002
	wmCommand    = 0x0111
	wmGetTextLen = 0x000E
	wmSetFont    = 0x0030
	wmDrawItem   = 0x002B
	wmEraseBkgnd = 0x0014
	wmNotify     = 0x004E

	colorWindow = 5

	idAddr, idSport, idToken = 1001, 1002, 1003
	idFrpc                   = 1004

	idAdd, idEditBtn, idDelBtn, idUp, idDown = 2005, 2006, 2007, 2008, 2009
	idStart, idStop, idShow, idClear         = 2001, 2002, 2003, 2004
	idDark                                   = 2011
	idBrowse                                 = 2010
	idLog, idStatus, idProxy, idList         = 3001, 3002, 3003, 3004

	// 自绘按钮样式
	bsOwnerDraw = 0x0000000B
)

// ListView / 通用控件消息
const (
	lvmFirst        = 0x1000
	lvmInsertItemW  = lvmFirst + 77  // 0x104D LVM_INSERTITEMW
	lvmDeleteAll    = lvmFirst + 9   // 0x1009 LVM_DELETEALLITEMS
	lvmSetItemTextW = lvmFirst + 116 // 0x1074 LVM_SETITEMTEXTW
	lvmGetNextItem  = lvmFirst + 12  // 0x100C LVM_GETNEXTITEM
	lvmInsertColW   = lvmFirst + 97  // 0x1061 LVM_INSERTCOLUMNW
	lvmSetItemState = lvmFirst + 43  // 0x102B LVM_SETITEMSTATE
	lvmSetExtStyle  = lvmFirst + 54  // 0x1036 LVM_SETEXTENDEDLISTVIEWSTYLE
	lvsReport      = 0x0001
	lvsShowSel     = 0x0008
	lvsExFullRow   = 0x0020
	lvsExGrid      = 0x0001
	lvsExDoubleBuf = 0x00010000
	lvFirst        = 0x1000
	lvifText       = 0x0001
	lvniSelected   = 0x0002
	lvcfmtLeft     = 0x0000
	lvcfmtFmt      = 0x0001
	iccListView    = 0x00000001
)

type lvColumn struct {
	mask     uint32
	fmt      int32
	cx       int32
	pszText  *uint16
	cchText  int32
	iSubItem int32
	order    int32
}

// LVITEMW 必须与 Win32 内存布局完全一致（64 位）：
// UINT mask; int iItem; int iSubItem; UINT state; UINT stateMask;
// LPWSTR pszText; int cchTextMax; int iImage; LPARAM lParam;
// int iIndent; int iGroupId; UINT cColumns; PUINT puColumns; int* piColFmt; int iGroup;
type lvItem struct {
	mask       uint32
	iItem      int32
	iSubItem   int32
	state      uint32
	stateMask  uint32
	pszText    *uint16
	cchTextMax int32
	iImage     int32
	lParam     uintptr
	iIndent    int32
	iGroupID   int32
	cColumns   uint32
	puColumns  uintptr
	piColFmt   uintptr
	iGroup     int32
}

type initCommonControlsEx struct {
	size uint32
	icc  uint32
}

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")

	pCreateWindowExW  = user32.NewProc("CreateWindowExW")
	pDefWindowProcW   = user32.NewProc("DefWindowProcW")
	pRegisterClassExW = user32.NewProc("RegisterClassExW")
	pGetMessageW      = user32.NewProc("GetMessageW")
	pPeekMessageW     = user32.NewProc("PeekMessageW")
	pTranslateMessage = user32.NewProc("TranslateMessage")
	pDispatchMessageW = user32.NewProc("DispatchMessageW")
	pPostQuitMessage  = user32.NewProc("PostQuitMessage")
	pSendMessageW     = user32.NewProc("SendMessageW")
	pSetWindowTextW   = user32.NewProc("SetWindowTextW")
	pGetWindowTextW   = user32.NewProc("GetWindowTextW")
	pGetWindowTextLen = user32.NewProc("GetWindowTextLengthW")
	pShowWindow       = user32.NewProc("ShowWindow")
	pUpdateWindow     = user32.NewProc("UpdateWindow")
	pInvalidateRect   = user32.NewProc("InvalidateRect")
	pMessageBoxW      = user32.NewProc("MessageBoxW")
	pDestroyWindow    = user32.NewProc("DestroyWindow")
	pEnableWindow     = user32.NewProc("EnableWindow")
	pCreateFontW      = gdi32.NewProc("CreateFontW")
	pGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")

	pIsWindow            = user32.NewProc("IsWindow")
	pGetWindowRect       = user32.NewProc("GetWindowRect")
	pSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	pSetFocus            = user32.NewProc("SetFocus")

	comctl32         = syscall.NewLazyDLL("comctl32.dll")
	pInitCommonCtrls = comctl32.NewProc("InitCommonControlsEx")

	comdlg32      = syscall.NewLazyDLL("comdlg32.dll")
	pGetOpenFileW = comdlg32.NewProc("GetOpenFileNameW")
	pCommDlgExtErr = comdlg32.NewProc("CommDlgExtendedError")

	ole32           = syscall.NewLazyDLL("ole32.dll")
	pOleInitialize  = ole32.NewProc("OleInitialize")

	shell32         = syscall.NewLazyDLL("shell32.dll")
	pDragAcceptFile = shell32.NewProc("DragAcceptFiles")
	pDragQueryFileW = shell32.NewProc("DragQueryFileW")
	pDragFinish     = shell32.NewProc("DragFinish")
)

// enableDragDrop 让主窗口接受文件拖入（拖 frpc.exe 进来即可设置路径）
func enableDragDrop(hwnd syscall.Handle) {
	pDragAcceptFile.Call(uintptr(hwnd), 1)
}

// onDropFiles 处理 WM_DROPFILES：取第一个拖入的文件
func onDropFiles(hDrop uintptr) {
	n, _, _ := pDragQueryFileW.Call(hDrop, ^uintptr(0), 0, 0)
	if n == 0 {
		pDragFinish.Call(hDrop)
		return
	}
	buf := make([]uint16, 1024)
	pDragQueryFileW.Call(hDrop, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	pDragFinish.Call(hDrop)
	p := syscall.UTF16ToString(buf)
	if p == "" {
		return
	}
	applyFrpcPath(p)
}

// OPENFILENAMEW：必须与 Win32 内存布局严格一致（64 位）
type openFileNameW struct {
	lStructSize       uint32
	hwndOwner         syscall.Handle
	hInstance         syscall.Handle
	lpstrFilter       *uint16
	lpstrCustomFilter *uint16
	nMaxCustFilter    uint32
	nFilterIndex      uint32
	lpstrFile         *uint16
	nMaxFile          uint32
	lpstrInitialDir   *uint16
	lpstrTitle        *uint16
	flags             uint32
	nFileOffset       uint16
	nFileExtension    uint16
	lpstrDefExt       *uint16
	lCustData         uintptr
	lpfnHook         uintptr
	lpTemplateName    *uint16
	pvReserved        uintptr
	dwReserved        uint32
	flagsEx           uint32
}

const (
	ofnReadOnly     = 0x00000001
	ofnNoChangeDir  = 0x00000008
	ofnPathMustExist = 0x00000800
	ofnFileMustExist = 0x00001000
	ofnExplorer     = 0x00080000
)

// appendUTF16 手写 UTF-16 编码：不能走 syscall.UTF16FromString，
// 因为它遇到字符串里的 NUL 会返回 EINVAL（过滤器串必须内嵌 NUL）。
func appendUTF16(dst []uint16, s string) []uint16 {
	for _, r := range s {
		if r < 0x10000 {
			dst = append(dst, uint16(r))
		} else {
			r -= 0x10000
			dst = append(dst, uint16(0xD800+(r>>10)), uint16(0xDC00+(r&0x3FF)))
		}
	}
	return dst
}

// buildFilter 拼过滤器：形如  "名称\0*.exe\0名称\0*.*\0\0"
func buildFilter(pairs [][2]string) []uint16 {
	var f []uint16
	for _, p := range pairs {
		f = appendUTF16(f, p[0])
		f = append(f, 0)
		f = appendUTF16(f, p[1])
		f = append(f, 0)
	}
	return append(f, 0) // 结尾必须是额外的 NUL
}

// pickExeFile 弹出系统「打开文件」对话框。
// 返回 ("路径", nil) 表示选好；("", nil) 表示用户取消；
// ("", err) 表示对话框本身打不开（可由安全软件拦截导致）。
//
// 注意：过滤器串必须内嵌 NUL，而 syscall.UTF16PtrFromString 遇到 NUL 会返回
// EINVAL。早期版本忽略了该错误，导致 lpstrFilter 变成 nil —— 这是对话框打不开的主因。
// 因此这里统一用 appendUTF16 手工编码。
func pickExeFile(owner syscall.Handle, initDir string) (string, error) {
	const bufLen = 1024
	buf := make([]uint16, bufLen)

	filter := buildFilter([][2]string{
		{"frpc 可执行文件 (frpc.exe)", "frpc.exe"},
		{"所有可执行文件 (*.exe)", "*.exe"},
		{"所有文件 (*.*)", "*.*"},
	})

	title := append(appendUTF16(nil, "请选择 frpc.exe"), 0)
	defExt := append(appendUTF16(nil, "exe"), 0)

	var dir []uint16
	if initDir != "" {
		dir = append(appendUTF16(dir, initDir), 0)
	}
	var dirPtr *uint16
	if len(dir) > 0 {
		dirPtr = &dir[0]
	}

	ofn := openFileNameW{
		lStructSize:     uint32(unsafe.Sizeof(openFileNameW{})),
		hwndOwner:       owner,
		lpstrFilter:     &filter[0],
		lpstrFile:       &buf[0],
		nMaxFile:        bufLen,
		lpstrInitialDir: dirPtr,
		lpstrTitle:      &title[0],
		lpstrDefExt:     &defExt[0],
		flags: ofnExplorer | ofnFileMustExist | ofnPathMustExist |
			ofnNoChangeDir | ofnReadOnly,
	}

	// 文件对话框内部会用到 shell/COM，先初始化
	pOleInitialize.Call(0)

	ok, _, _ := pGetOpenFileW.Call(uintptr(unsafe.Pointer(&ofn)))

	// 这些缓冲必须活到调用返回之后
	runtime.KeepAlive(buf)
	runtime.KeepAlive(filter)
	runtime.KeepAlive(title)
	runtime.KeepAlive(defExt)
	runtime.KeepAlive(dir)
	runtime.KeepAlive(ofn)

	if ok != 0 {
		return syscall.UTF16ToString(buf), nil
	}

	// 返回 0 有两种可能：用户点了取消，或对话框初始化失败。
	// CommDlgExtendedError 非 0 说明是后一种，必须告诉用户，不能静默无反应。
	if cde, _, _ := pCommDlgExtErr.Call(); cde != 0 {
		return "", fmt.Errorf("文件对话框打开失败（错误码 %d）", cde)
	}
	return "", nil // 用户取消
}

// 公共控件类别位
const (
	iccWin95Classes    = 0x000000FF // 动画/列表/树/进度/状态栏/标签/工具提示等
	iccStandardClasses = 0x00004000 // 按钮/编辑框/静态框/列表框/组合框/滚动条
)

func initCommonControls() {
	// 「打开文件」对话框在 Explorer 模式下会用到 ListView / TreeView / 按钮等，
	// 只加载 ICC_LISTVIEW_CLASSES 不够，可能导致 CDERR_INITIALIZATION。
	e := initCommonControlsEx{
		size: uint32(unsafe.Sizeof(initCommonControlsEx{})),
		icc:  iccWin95Classes | iccStandardClasses,
	}
	pInitCommonCtrls.Call(uintptr(unsafe.Pointer(&e)))
}

// lvInsert 插入一行（4 列：名称 / 本地 / 远程 / 状态）
func lvInsert(idx int, cols [5]string) {
	// 这些 UTF-16 缓冲区必须存活到 SendMessage 返回之后
	keep := make([]*uint16, 5)
	for i := 0; i < 5; i++ {
		keep[i] = u16(cols[i])
	}
	defer runtime.KeepAlive(keep)

	// 一次调用写入全部列：LVITEM.iSubItem=0 插行，随后逐列 SetItemText。
	// 关键：LVITEM 的 a mask 必须是 LVIF_TEXT，且 pszText 指向的缓冲要一直有效。
	for c := 0; c < 5; c++ {
		it := lvItem{
			mask:       lvifText,
			iItem:      int32(idx),
			iSubItem:   int32(c),
			pszText:    keep[c],
			cchTextMax: int32(len([]rune(cols[c])) + 1),
		}
		if c == 0 {
			// 注意：LVM_INSERTITEMW 成功时返回新行的下标，插第 0 行返回 0，
			// 不能用 ==0 判断失败，否则第 0 行的子列永远不会被写入。
			pSendMessageW.Call(uintptr(hList), lvmInsertItemW, 0, uintptr(unsafe.Pointer(&it)))
			continue
		}
		// wParam = 行号，lParam = LVITEM 指针（子列号在 LVITEM.iSubItem 里）
		pSendMessageW.Call(uintptr(hList), lvmSetItemTextW, uintptr(idx), uintptr(unsafe.Pointer(&it)))
	}
}

func lvClear() {
	pSendMessageW.Call(uintptr(hList), lvmDeleteAll, 0, 0)
}

func lvSelected() int {
	// LVM_GETNEXTITEM 用 -1 作为起始，返回选中项下标
	r, _, _ := pSendMessageW.Call(uintptr(hList), lvmGetNextItem, ^uintptr(0), lvniSelected)
	return int(int32(r))
}

func refreshList() {
	lvClear()
	for i, p := range proxies {
		p.setDefaults()
		lvInsert(i, [5]string{
			p.Name,
			p.Type,
			p.LocalIP + ":" + p.LocalPort,
			p.targetText(),
			"待启动",
		})
	}
}

func setRowStatus(i int, s string) {
	if i < 0 || i >= len(proxies) {
		return
	}
	t := u16(s)
	it := lvItem{mask: lvifText, iItem: int32(i), iSubItem: 4, pszText: t}
	pSendMessageW.Call(uintptr(hList), lvmSetItemTextW, uintptr(i), uintptr(unsafe.Pointer(&it)))
}

func resetRowStatus() {
	for i := range proxies {
		setRowStatus(i, "待启动")
	}
}

// setSel 选中指定行
func setSel(i int) {
	if i < 0 {
		return
	}
	it := lvItem{mask: 0, iItem: int32(i), state: lvniSelected, stateMask: lvniSelected}
	pSendMessageW.Call(uintptr(hList), lvmSetItemState, uintptr(i), uintptr(unsafe.Pointer(&it)))
}

type wndClassEx struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     syscall.Handle
	hIcon         syscall.Handle
	hCursor       syscall.Handle
	hbrBackground syscall.Handle
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       syscall.Handle
}

type msg struct {
	hwnd    syscall.Handle
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      struct{ x, y int32 }
}

type rect struct{ left, top, right, bottom int32 }

type config struct {
	ServerAddr string   `json:"server_addr"`
	ServerPort string   `json:"server_port"`
	Token      string   `json:"token"`
	FrpcPath   string   `json:"frpc_path"` // 用户选择的 frpc.exe 路径
	DarkMode   bool     `json:"dark_mode"` // 深色模式
	Proxies    []*proxy `json:"proxies"`
}

// proxy 一条 TCP 代理
type proxy struct {
	Name       string `json:"name"`
	Type       string `json:"type"` // tcp / udp / http / https
	LocalIP    string `json:"local_ip"`
	LocalPort  string `json:"local_port"`
	RemotePort string `json:"remote_port"` // tcp / udp 用
	// http / https 用：自定义域名，可多个
	CustomDomains []string `json:"custom_domains,omitempty"`
}

// 支持的代理类型
var proxyTypes = []string{"tcp", "udp", "http", "https"}

func validProxyType(t string) bool {
	for _, x := range proxyTypes {
		if x == t {
			return true
		}
	}
	return false
}

// useRemotePort 该类型是否用「远程端口」（否则用自定义域名）
func (p *proxy) useRemotePort() bool {
	return p.Type == "tcp" || p.Type == "udp"
}

// domainText 把域名列表拼成逗号分隔的字符串（界面显示用）
func (p *proxy) domainText() string { return strings.Join(p.CustomDomains, ", ") }

// targetText 列表里「远程端口 / 域名」那一列显示什么
func (p *proxy) targetText() string {
	if p.useRemotePort() {
		return ":" + p.RemotePort
	}
	return p.domainText()
}

// splitDomains 解析逗号/分号/空白分隔的域名串，去重去空
func splitDomains(s string) []string {
	f := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n'
	})
	seen := map[string]bool{}
	var out []string
	for _, d := range f {
		d = strings.TrimSpace(d)
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}

func (p *proxy) setDefaults() {
	if !validProxyType(p.Type) {
		// 旧配置没有 type 字段，默认 tcp
		p.Type = "tcp"
	}
	if strings.TrimSpace(p.Name) == "" {
		p.Name = p.Type
	}
	if strings.TrimSpace(p.LocalIP) == "" {
		p.LocalIP = "127.0.0.1"
	}
}

func (c *config) setDefaults() {
	if c.ServerAddr == "" {
		c.ServerAddr = "127.0.0.1"
	}
	if c.ServerPort == "" {
		c.ServerPort = "7000"
	}
	for _, p := range c.Proxies {
		p.setDefaults()
	}
}

// workDir 返回程序自己的运行时目录：exe 同级下的 frpc-gui-data。
// 不用 %TEMP%——某些安全软件会拦截向系统临时目录释放可执行文件。
// 若 exe 所在目录不可写（如放在 C:\Program Files），回退到 %LOCALAPPDATA%。
func workDir() string {
	base := exeDir()
	d := filepath.Join(base, "frpc-gui-data")
	if err := os.MkdirAll(d, 0o755); err == nil {
		if probeWritable(d) {
			return d
		}
	}
	// 回退：%LOCALAPPDATA%\frpc-gui
	al := os.Getenv("LOCALAPPDATA")
	if al == "" {
		al = os.Getenv("APPDATA")
	}
	if al == "" {
		al = os.TempDir()
	}
	d2 := filepath.Join(al, "frpc-gui")
	os.MkdirAll(d2, 0o755)
	return d2
}

func exeDir() string {
	p, err := os.Executable()
	if err != nil {
		wd, _ := os.Getwd()
		return wd
	}
	return filepath.Dir(p)
}

func probeWritable(dir string) bool {
	f := filepath.Join(dir, ".wtest")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		return false
	}
	os.Remove(f)
	return true
}

var workDirOnce sync.Once
var workDirPath string

func W() string {
	workDirOnce.Do(func() { workDirPath = workDir() })
	return workDirPath
}

// 自动补全代理名称，保证不重复
func (c *config) uniqueName(base string) string {
	c.setDefaults()
	if strings.TrimSpace(base) == "" {
		base = "tcp"
	}
	used := map[string]bool{}
	for _, p := range c.Proxies {
		used[p.Name] = true
	}
	if !used[base] {
		return base
	}
	for i := 2; ; i++ {
		n := fmt.Sprintf("%s-%d", base, i)
		if !used[n] {
			return n
		}
	}
}

var (
	hMain, hLog, hStatus, hProxyInfo               syscall.Handle
	hAddr, hSport, hToken, hFrpc                   syscall.Handle
	hList                                          syscall.Handle
	hStart, hStop                                  syscall.Handle

	hFont   = uintptr(0)
	showPwd atomic.Bool

	proxies  []*proxy
	editIdx  = -1 // 正在编辑的代理下标，-1 表示新增
	selIdx   = -1 // ListView 当前选中行

	proc     *exec.Cmd
	procBusy atomic.Bool
	runNames []string

	logQueue   = make(chan string, 1024)
	proxyQueue = make(chan string, 64)
	rowQueue   = make(chan rowMsg, 128)
	exitQueue  = make(chan int, 8)
)

func u16(s string) *uint16 {
	p, _ := syscall.UTF16PtrFromString(s)
	return p
}

func getText(h syscall.Handle) string {
	n, _, _ := pGetWindowTextLen.Call(uintptr(h))
	if n == 0 {
		return ""
	}
	buf := make([]uint16, n+1)
	pGetWindowTextW.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), uintptr(n+1))
	return syscall.UTF16ToString(buf)
}

func setText(h syscall.Handle, s string) {
	pSetWindowTextW.Call(uintptr(h), uintptr(unsafe.Pointer(u16(s))))
}

func msgBox(body string, flags uintptr) {
	pMessageBoxW.Call(uintptr(hMain), uintptr(unsafe.Pointer(u16(body))),
		uintptr(unsafe.Pointer(u16(appName))), flags)
}

func getInstance() syscall.Handle {
	h, _, _ := pGetModuleHandleW.Call(0)
	return syscall.Handle(h)
}

func mk(class, text string, style uintptr, id, x, y, w, h int) syscall.Handle {
	hw, _, _ := pCreateWindowExW.Call(0,
		uintptr(unsafe.Pointer(u16(class))),
		uintptr(unsafe.Pointer(u16(text))),
		style, uintptr(ds(x)), uintptr(ds(y)), uintptr(ds(w)), uintptr(ds(h)),
		uintptr(hMain), uintptr(id), uintptr(getInstance()), 0)
	if hw != 0 && hFont != 0 {
		pSendMessageW.Call(hw, wmSetFont, hFont, 1)
	}
	return syscall.Handle(hw)
}

// mkNoFont 创建控件但不套用自定义字体（ListView 需要系统默认字体才能正常显示子列）
func mkNoFont(class, text string, style uintptr, id, x, y, w, h int) syscall.Handle {
	hw, _, _ := pCreateWindowExW.Call(0,
		uintptr(unsafe.Pointer(u16(class))),
		uintptr(unsafe.Pointer(u16(text))),
		style, uintptr(ds(x)), uintptr(ds(y)), uintptr(ds(w)), uintptr(ds(h)),
		uintptr(hMain), uintptr(id), uintptr(getInstance()), 0)
	return syscall.Handle(hw)
}

func mkInput(x, y, w, id int, pw bool) syscall.Handle {
	st := uintptr(wsChild | wsVisible | wsTabStop | wsBorder | esAutoHScroll)
	if pw {
		st |= esPassword
	}
	return mk("EDIT", "", st, id, x, y, w, 24)
}

func log(s string) {
	if hLog == 0 {
		return
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\n", "\r\n")
	cur, _, _ := pSendMessageW.Call(uintptr(hLog), wmGetTextLen, 0, 0)
	pSendMessageW.Call(uintptr(hLog), emSetSel, cur, cur)
	pSendMessageW.Call(uintptr(hLog), emReplaceSel, 0, uintptr(unsafe.Pointer(u16(s+"\r\n"))))
}

func postLog(s string) {
	select {
	case logQueue <- s:
	default:
	}
}

func postProxy(s string) {
	select {
	case proxyQueue <- s:
	default:
	}
}

func postExit(code int) {
	select {
	case exitQueue <- code:
	default:
	}
}

func configPath() string { return filepath.Join(W(), "config.json") }

func loadCfg() *config {
	c := &config{}
	if b, err := os.ReadFile(configPath()); err == nil {
		json.Unmarshal(b, c)
	}
	c.setDefaults()
	return c
}

func saveCfg(c *config) {
	if b, err := json.MarshalIndent(c, "", "  "); err == nil {
		os.WriteFile(configPath(), b, 0o644)
	}
}

// findFrpcAuto 自动探测 frpc.exe：exe 同级 → 同级 frp 子目录 → PATH
func findFrpcAuto() string {
	base := exeDir()
	cands := []string{
		filepath.Join(base, "frpc.exe"),
		filepath.Join(base, "frp", "frpc.exe"),
		filepath.Join(base, "..", "frpc.exe"),
	}
	// 复用已保存的路径
	if p := loadCfg().FrpcPath; p != "" {
		cands = append([]string{p}, cands...)
	}
	for _, c := range cands {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			abs, _ := filepath.Abs(c)
			return abs
		}
	}
	// 最后尝试 PATH
	if p, err := exec.LookPath("frpc.exe"); err == nil {
		return p
	}
	return ""
}

// checkFrpcFile 校验选中的文件是不是可用的 frpc
func checkFrpcFile(p string) error {
	st, err := os.Stat(p)
	if err != nil {
		return fmt.Errorf("文件不存在：%s", p)
	}
	if st.IsDir() {
		return fmt.Errorf("这是一个目录，不是文件：%s", p)
	}
	if !strings.EqualFold(filepath.Ext(p), ".exe") {
		return fmt.Errorf("请选择 frpc.exe 文件")
	}
	// 大小写个下限，避免误选到同名小文件
	if st.Size() < 1<<20 {
		return fmt.Errorf("文件太小（%.1f MB），不像 frpc.exe", float64(st.Size())/(1<<20))
	}
	return nil
}

// tomlPath 生成配置文件的路径
func tomlPath() string { return filepath.Join(W(), "frpc.generated.toml") }

func esc(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	return strings.ReplaceAll(s, "\"", "\\\"")
}

func buildToml(c *config) string {
	var b strings.Builder
	fmt.Fprintf(&b, "serverAddr = \"%s\"\r\n", esc(c.ServerAddr))
	fmt.Fprintf(&b, "serverPort = %s\r\n", c.ServerPort)
	if strings.TrimSpace(c.Token) != "" {
		b.WriteString("auth.method = \"token\"\r\n")
		fmt.Fprintf(&b, "auth.token = \"%s\"\r\n", esc(c.Token))
	}
	for _, p := range c.Proxies {
		b.WriteString("\r\n[[proxies]]\r\n")
		fmt.Fprintf(&b, "name = \"%s\"\r\n", esc(p.Name))
		fmt.Fprintf(&b, "type = \"%s\"\r\n", esc(p.Type))
		fmt.Fprintf(&b, "localIP = \"%s\"\r\n", esc(p.LocalIP))
		fmt.Fprintf(&b, "localPort = %s\r\n", p.LocalPort)
		if p.useRemotePort() {
			// tcp / udp：用远程端口
			fmt.Fprintf(&b, "remotePort = %s\r\n", p.RemotePort)
		} else {
			// http / https：用自定义域名
			quoted := make([]string, 0, len(p.CustomDomains))
			for _, d := range p.CustomDomains {
				quoted = append(quoted, "\""+esc(d)+"\"")
			}
			fmt.Fprintf(&b, "customDomains = [%s]\r\n", strings.Join(quoted, ", "))
		}
	}
	return b.String()
}

func validatePort(v, label string) (string, error) {
	v = strings.TrimSpace(v)
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 || n > 65535 {
		return "", fmt.Errorf("%s 必须是 1-65535 的数字", label)
	}
	return v, nil
}

func collect() (*config, error) {
	c := &config{
		ServerAddr: strings.TrimSpace(getText(hAddr)),
		ServerPort: getText(hSport),
		Token:      getText(hToken),
		FrpcPath:   strings.TrimSpace(getText(hFrpc)),
		DarkMode:   darkMode,
		Proxies:    proxies,
	}
	if c.ServerAddr == "" {
		return nil, fmt.Errorf("服务器 IP 不能为空")
	}
	if c.FrpcPath == "" {
		return nil, fmt.Errorf("请先选择 frpc.exe 的位置")
	}
	if err := checkFrpcFile(c.FrpcPath); err != nil {
		return nil, err
	}
	var err error
	if c.ServerPort, err = validatePort(c.ServerPort, "服务器端口"); err != nil {
		return nil, err
	}
	if len(c.Proxies) == 0 {
		return nil, fmt.Errorf("至少要添加一条代理")
	}
	seen := map[string]bool{}
	for i, p := range c.Proxies {
		p.setDefaults()
		no := i + 1
		if p.LocalPort, err = validatePort(p.LocalPort, fmt.Sprintf("第 %d 条代理的本地端口", no)); err != nil {
			return nil, err
		}
		if p.useRemotePort() {
			if p.RemotePort, err = validatePort(p.RemotePort, fmt.Sprintf("第 %d 条代理的远程端口", no)); err != nil {
				return nil, err
			}
		} else {
			p.CustomDomains = splitDomains(strings.Join(p.CustomDomains, ","))
			if len(p.CustomDomains) == 0 {
				return nil, fmt.Errorf("第 %d 条 %s 代理至少要填一个自定义域名", no, p.Type)
			}
		}
		if seen[p.Name] {
			return nil, fmt.Errorf("代理名称重复：%s", p.Name)
		}
		seen[p.Name] = true
	}
	return c, nil
}

func setStatus(s string, running bool) {
	setText(hStatus, s)
	if running {
		pEnableWindow.Call(uintptr(hStart), 0)
		pEnableWindow.Call(uintptr(hStop), 1)
	} else {
		pEnableWindow.Call(uintptr(hStart), 1)
		pEnableWindow.Call(uintptr(hStop), 0)
	}
}

func startFrpc() {
	if !procBusy.CompareAndSwap(false, true) {
		msgBox("已在运行中。", 0x40)
		return
	}
	defer procBusy.Store(false)

	c, err := collect()
	if err != nil {
		msgBox(err.Error(), 0x10)
		return
	}
	saveCfg(c)

	exe := c.FrpcPath
	cfgFile := tomlPath()
	if err := os.WriteFile(cfgFile, []byte(buildToml(c)), 0o644); err != nil {
		msgBox("写入配置失败：\r\n"+err.Error(), 0x10)
		return
	}

	setText(hLog, "")
	resetRowStatus()
	log(fmt.Sprintf("[启动] %s -c %s", exe, cfgFile))
	log(fmt.Sprintf("[信息] 共 %d 条代理", len(c.Proxies)))
	shown := buildToml(c)
	if c.Token != "" {
		shown = strings.ReplaceAll(shown, c.Token, "******")
	}
	log("--- 生成的配置 ---")
	for _, ln := range strings.Split(shown, "\r\n") {
		if strings.TrimSpace(ln) != "" {
			log("  " + ln)
		}
	}
	log("------------------")

	cmd := exec.Command(exe, "-c", cfgFile)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		msgBox("创建管道失败："+err.Error(), 0x10)
		return
	}
	cmd.Stderr = cmd.Stdout

	if err := cmd.Start(); err != nil {
		msgBox("启动 frpc 失败：\r\n"+err.Error(), 0x10)
		return
	}

	proc = cmd
	setStatus("● 运行中", true)
	postLog("[ok] frpc 已启动 (PID " + strconv.Itoa(cmd.Process.Pid) + ")")

	go func() {
		buf := make([]byte, 4096)
		var acc string
		for {
			n, rerr := stdout.Read(buf)
			if n > 0 {
				acc += string(buf[:n])
				for {
					i := strings.IndexAny(acc, "\r\n")
					if i < 0 {
						break
					}
					line := strings.TrimSpace(acc[:i])
					acc = acc[i+1:]
					if line != "" && !strings.Contains(line, "GAMEPP") {
						postLog(line)
						trackProxy(line)
					}
				}
			}
			if rerr != nil {
				break
			}
		}
		code := 0
		if werr := cmd.Wait(); werr != nil {
			if ee, ok := werr.(*exec.ExitError); ok {
				code = ee.ExitCode()
			}
		}
		postExit(code)
	}()
}

func trackProxy(line string) {
	lower := strings.ToLower(line)
	if !strings.Contains(lower, "start proxy success") && !strings.Contains(lower, "proxy added") {
		return
	}
	// 日志形如：[test-tcp] start proxy success
	name := ""
	if i := strings.Index(line, "["); i >= 0 {
		if j := strings.Index(line[i:], "]"); j > 0 {
			name = line[i+1 : i+j]
		}
	}
	for i, p := range proxies {
		if p.Name == name {
			postRow(i, "✔ 已启动")
			return
		}
	}
}

func postRow(i int, s string) {
	select {
	case rowQueue <- rowMsg{i, s}:
	default:
	}
}

type rowMsg struct {
	i int
	s string
}

func stopFrpc() {
	p := proc
	if p == nil || p.Process == nil {
		return
	}
	postLog("[停止] 正在终止 frpc…")
	_ = p.Process.Kill()
}

func toggleToken() {
	// EM_SETPASSWORDCHAR 改了字符之后控件不会自动重绘，必须手动 Invalidate，
	// 否则看起来“勾了显示但 token 还是点点点”。
	if showPwd.Load() {
		showPwd.Store(false)
		pSendMessageW.Call(uintptr(hToken), emSetPwdChar, 0x25CF, 0)
	} else {
		showPwd.Store(true)
		pSendMessageW.Call(uintptr(hToken), emSetPwdChar, 0, 0)
	}
	// 强制整个客户区重绘，并刷新
	pInvalidateRect.Call(uintptr(hToken), 0, 1)
	pUpdateWindow.Call(uintptr(hToken))
	setCheck(hShowChk, showPwd.Load())
}

const wmDropFiles = 0x0233

func wndProc(hwnd syscall.Handle, m uint32, w uintptr, l unsafe.Pointer) uintptr {
	switch m {
	case wmDropFiles:
		onDropFiles(w)
		return 0
	case wmDrawItem:
		return drawOwnerItem(l)
	case wmEraseBkgnd:
		if r := eraseBackground(hwnd, syscall.Handle(w)); r != 0 {
			return r
		}
	case wmCtlColorBtn, wmCtlColorEdit, wmCtlColorListBox, wmCtlColorStatic:
		if br := ctlColor(m, syscall.Handle(w), l); br != 0 {
			return br
		}
	case wmCommand:
		switch int(w & 0xffff) {
		case idStart:
			go startFrpc()
		case idStop:
			go stopFrpc()
		case idShow:
			toggleToken()
		case idDark:
			onToggleDark()
		case idClear:
			setText(hLog, "")
		case idBrowse:
			onBrowseFrpc()
		case idAdd:
			onAddProxy()
		case idEditBtn:
			onEditProxy()
		case idDelBtn:
			onDeleteProxy()
		case idUp:
			onMoveProxy(-1)
		case idDown:
			onMoveProxy(+1)
		}
		return 0
	case wmClose:
		if proc != nil && proc.Process != nil {
			_ = proc.Process.Kill()
		}
		pDestroyWindow.Call(uintptr(hwnd))
		return 0
	case wmDestroy:
		pPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := pDefWindowProcW.Call(uintptr(hwnd), uintptr(m), w, uintptr(l))
	return r
}

func onAddProxy() {
	p := showEditDialog(nil)
	if p == nil {
		return
	}
	c := &config{Proxies: proxies}
	p.Name = c.uniqueName(p.Name)
	proxies = append(proxies, p)
	refreshList()
	setSel(len(proxies) - 1)
	log("[添加] " + p.Name)
}

func onEditProxy() {
	i := lvSelected()
	if i < 0 || i >= len(proxies) {
		msgBox("请先在列表中选中一条代理。", 0x40)
		return
	}
	p := showEditDialog(proxies[i])
	if p == nil {
		return
	}
	proxies[i] = p
	refreshList()
	setSel(i)
	log("[修改] " + p.Name)
}

func onDeleteProxy() {
	i := lvSelected()
	if i < 0 || i >= len(proxies) {
		msgBox("请先在列表中选中一条代理。", 0x40)
		return
	}
	mbRet, _, _ := pMessageBoxW.Call(uintptr(hMain),
		uintptr(unsafe.Pointer(u16("确定删除代理 “"+proxies[i].Name+"” 吗？"))),
		uintptr(unsafe.Pointer(u16(appName))), 0x34)
	if mbRet != 6 { // IDYES
		return
	}
	log("[删除] " + proxies[i].Name)
	proxies = append(proxies[:i], proxies[i+1:]...)
	refreshList()
	if len(proxies) > 0 {
		setSel(clamp(i, 0, len(proxies)-1))
	}
}

func onMoveProxy(dir int) {
	i := lvSelected()
	if i < 0 || i >= len(proxies) {
		msgBox("请先在列表中选中一条代理。", 0x40)
		return
	}
	j := i + dir
	if j < 0 || j >= len(proxies) {
		return
	}
	proxies[i], proxies[j] = proxies[j], proxies[i]
	refreshList()
	setSel(j)
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// onBrowseFrpc 让用户选择 frpc.exe 的位置
func onBrowseFrpc() {
	cur := strings.TrimSpace(getText(hFrpc))
	initDir := ""
	if cur != "" {
		initDir = filepath.Dir(cur)
	} else {
		initDir = exeDir()
	}
	// 优先用现代 COM 对话框（IFileOpenDialog）；
	// 老 API GetOpenFileNameW 在 Win11 24H2 上实测打不开，仅作兜底。
	p, err := pickExeFileModern(hMain, "请选择 frpc.exe")
	if err != nil {

		log("[警告] 现代文件对话框不可用，改用备用方式: " + err.Error())
		p, err = pickExeFile(hMain, initDir)
	}
	if err != nil {
		msgBox(err.Error(), 0x30)
		return
	}
	if p == "" {
		return // 用户取消
	}
	applyFrpcPath(p)
}

// applyFrpcPath 校验并采用一个 frpc.exe 路径（浏览、拖拽共用）
func applyFrpcPath(p string) {
	if err := checkFrpcFile(p); err != nil {
		msgBox(err.Error(), 0x10)
		return
	}
	setText(hFrpc, p)
	log("[已选择] frpc.exe → " + p)
	// 立即记住，下次直接可用
	if c := collectQuiet(); c != nil {
		saveCfg(c)
	}
}

// collectQuiet 读取当前界面值但忽略校验错误，用于仅保存配置
func collectQuiet() *config {
	if hAddr == 0 {
		return nil
	}
	return &config{
		ServerAddr: strings.TrimSpace(getText(hAddr)),
		ServerPort: strings.TrimSpace(getText(hSport)),
		Token:      getText(hToken),
		FrpcPath:   strings.TrimSpace(getText(hFrpc)),
		DarkMode:   darkMode,
		Proxies:    proxies,
	}
}

func pumpQueues() {
	for {
		select {
		case s := <-logQueue:
			log(s)
		case m := <-rowQueue:
			setRowStatus(m.i, m.s)
		case code := <-exitQueue:
			if code == 0 {
				log("[退出] frpc 已正常结束")
			} else {
				log(fmt.Sprintf("[退出] frpc 异常结束（退出码 %d）", code))
				log("[提示] 常见原因：服务端未启动、服务端 7000 端口被防火墙/云安全组拦截、或 token 不匹配。")
			}
			proc = nil
			setStatus("● 已停止", false)
			resetRowStatus()
		default:
			return
		}
	}
}

// wndProcPtr 必须保存在包级变量中，否则回调可能被 GC 回收导致访问违例
var wndProcPtr = syscall.NewCallback(wndProc)

// editProcPtr 同理，必须持有引用
var editProcPtr = syscall.NewCallback(editProc)

func registerClass(name string, proc uintptr) bool {
	wc := wndClassEx{
		cbSize:        uint32(unsafe.Sizeof(wndClassEx{})),
		lpfnWndProc:   proc,
		hInstance:     getInstance(),
		hbrBackground: syscall.Handle(colorWindow + 1),
		lpszClassName: u16(name),
	}
	r, _, _ := pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	return r != 0
}

func buildUI(inst syscall.Handle) {
	initCommonControls()
	const cls = "FrpcGuiWnd"
	if !registerClass(cls, wndProcPtr) {
		msgBox("注册窗口类失败", 0x10)
		return
	}
	registerClass(editCls, editProcPtr)

	hFont = mkFont(9)
	if hFont == 0 {
		msgBox("字体创建失败，界面文字可能显示异常", 0x30)
	}

	const style = uintptr(wsOverlapped | wsCaption | wsSysMenu | wsVisible | wsMinimize)
	h, _, err := pCreateWindowExW.Call(0,
		uintptr(unsafe.Pointer(u16(cls))),
		uintptr(unsafe.Pointer(u16(appName))),
		style, uintptr(ds(60)), uintptr(ds(60)), uintptr(ds(700)), uintptr(ds(730)), 0, 0, uintptr(inst), 0)
	hMain = syscall.Handle(h)
	if hMain == 0 {
		msgBox("窗口创建失败: "+err.Error(), 0x10)
		return
	}

	// ---- 连接设置 ----
	mk("BUTTON", "服务端", uintptr(wsChild|wsVisible|bsGroupBox), 0, 14, 8, 660, 106)

	mk("STATIC", "服务器 IP", uintptr(wsChild|wsVisible), 0, 30, 36, 64, 22)
	hAddr = mkInput(100, 32, 180, idAddr, false)
	mk("STATIC", "端口", uintptr(wsChild|wsVisible), 0, 300, 36, 34, 22)
	hSport = mkInput(340, 32, 80, idSport, false)

	mk("STATIC", "Token", uintptr(wsChild|wsVisible), 0, 30, 74, 64, 22)
	hToken = mk("EDIT", "", uintptr(wsChild|wsVisible|wsTabStop|wsBorder|esAutoHScroll|esPassword), idToken, 100, 70, 320, 24)
	hShowChk = mk("BUTTON", "显示", uintptr(wsChild|wsVisible|wsTabStop|bsOwnerDraw|0x00000003), idShow, 430, 70, 70, 24)
	hDarkChk = mk("BUTTON", "深色", uintptr(wsChild|wsVisible|wsTabStop|bsOwnerDraw|0x00000003), idDark, 508, 70, 76, 24)

	// ---- frpc.exe 位置（本程序不再内嵌，需用户自行指定）----
	mk("BUTTON", "frpc.exe 位置", uintptr(wsChild|wsVisible|bsGroupBox), 0, 14, 120, 660, 62)
	mk("STATIC", "程序路径", uintptr(wsChild|wsVisible), 0, 30, 148, 64, 22)
	hFrpc = mkInput(100, 144, 430, idFrpc, false)
	mk("BUTTON", "浏览…", uintptr(wsChild|wsVisible|wsTabStop|bsOwnerDraw), idBrowse, 540, 143, 86, 26)

	// ---- 代理列表 ----
	mk("BUTTON", "代理列表（可添加多条 TCP）", uintptr(wsChild|wsVisible|bsGroupBox), 0, 14, 188, 660, 186)

	hList = mkNoFont("SysListView32", "", uintptr(wsChild|wsVisible|wsBorder|wsTabStop|lvsReport|lvsShowSel), idList, 28, 214, 632, 116)
	pSendMessageW.Call(uintptr(hList), lvmSetExtStyle, uintptr(lvsExFullRow|lvsExGrid), uintptr(lvsExFullRow|lvsExGrid))
	addColumn(0, "名称", 120)
	addColumn(1, "类型", 70)
	addColumn(2, "本地地址", 150)
	addColumn(3, "远程端口 / 域名", 190)
	addColumn(4, "状态", 100)
	// 列表内容在窗口显示后填充（见 main），此处不填

	mk("BUTTON", "添加", uintptr(wsChild|wsVisible|wsTabStop|bsOwnerDraw), idAdd, 28, 336, 80, 30)
	mk("BUTTON", "修改", uintptr(wsChild|wsVisible|wsTabStop|bsOwnerDraw), idEditBtn, 114, 336, 80, 30)
	mk("BUTTON", "删除", uintptr(wsChild|wsVisible|wsTabStop|bsOwnerDraw), idDelBtn, 200, 336, 80, 30)
	mk("BUTTON", "上移", uintptr(wsChild|wsVisible|wsTabStop|bsOwnerDraw), idUp, 286, 336, 70, 30)
	mk("BUTTON", "下移", uintptr(wsChild|wsVisible|wsTabStop|bsOwnerDraw), idDown, 362, 336, 70, 30)

	// ---- 按钮 ----
	hStart = mk("BUTTON", "保存并启动", uintptr(wsChild|wsVisible|wsTabStop|bsOwnerDraw), idStart, 18, 382, 120, 34)
	hStop = mk("BUTTON", "停止", uintptr(wsChild|wsVisible|wsTabStop|bsOwnerDraw), idStop, 146, 382, 88, 34)
	pEnableWindow.Call(uintptr(hStop), 0)
	mk("BUTTON", "清空日志", uintptr(wsChild|wsVisible|wsTabStop|bsOwnerDraw), idClear, 242, 382, 92, 34)
	hStatus = mk("STATIC", "● 未运行", uintptr(wsChild|wsVisible), idStatus, 570, 392, 100, 22)
	hProxyInfo = mk("STATIC", "", uintptr(wsChild|wsVisible), idProxy, 346, 392, 216, 22)

	// ---- 日志 ----
	mk("STATIC", "日志", uintptr(wsChild|wsVisible), 0, 18, 424, 60, 20)
	hLog = mk("EDIT", "",
		uintptr(wsChild|wsVisible|wsBorder|wsVScroll|esLeft|esMultiline|esAutoHScroll|esReadOnly),
		idLog, 18, 446, 660, 190)

	setStatus("● 未运行", false)
}

func addColumn(i int, title string, w int) {
	// LVCF_FMT(0x1) | LVCF_WIDTH(0x2) | LVCF_TEXT(0x4) | LVCF_SUBITEM(0x8) = 0xF
	t := u16(title)
	c := lvColumn{mask: 0x000F, fmt: lvcfmtLeft, cx: int32(ds(w)), pszText: t, iSubItem: int32(i)}
	pSendMessageW.Call(uintptr(hList), lvmInsertColW, uintptr(i), uintptr(unsafe.Pointer(&c)))
	runtime.KeepAlive(t)   // 保证 title 的 UTF-16 缓冲在调用期间存活
	runtime.KeepAlive(c)
}

// ---------------- 代理编辑对话框 ----------------

const (
	didOK, didCancel                          = 4001, 4002
	didName, didLip, didLport, didRport       = 4003, 4004, 4005, 4006
	didType, didDomains                       = 4007, 4008
	didTypeHint, didRportHint, didDomainsHint = 4009, 4010, 4011
)

var editCls = "FrpcEditDlg"

// dlgType 当前对话框里选中的代理类型
func dlgTypeNow() string {
	if dlgTypeH == 0 {
		return "tcp"
	}
	// CB_GETCURSEL = 0x0147
	i, _, _ := pSendMessageW.Call(uintptr(dlgTypeH), 0x0147, 0, 0)
	if int32(i) < 0 || int(i) >= len(proxyTypes) {
		return "tcp"
	}
	return proxyTypes[i]
}

// updateDlgEnabled 按类型切换「远程端口」与「自定义域名」两组控件。
// 两组控件坐标相同，必须真正显示/隐藏而不是只启用/禁用，否则会互相遮挡。
func updateDlgEnabled() {
	usePort := dlgTypeNow() == "tcp" || dlgTypeNow() == "udp"

	showCtl(dlgRportHint, usePort)
	showCtl(dlgRport, usePort)
	showCtl(dlgDomainsHint, !usePort)
	showCtl(dlgDomains, !usePort)
	showCtl(dlgDomainTip, !usePort)
}

func showCtl(h syscall.Handle, on bool) {
	if h == 0 {
		return
	}
	cmd := uintptr(0) // SW_HIDE
	if on {
		cmd = 5 // SW_SHOW
	}
	pShowWindow.Call(uintptr(h), cmd)
}

func editProc(hwnd syscall.Handle, m uint32, w uintptr, l unsafe.Pointer) uintptr {
	if m == wmCommand {
		switch int(w & 0xffff) {
		case didType:
			// CBN_SELCHANGE = 1
			if int(w>>16) == 1 {
				updateDlgEnabled()
			}
			return 0
		case didOK:
			if err := validateEditFields(hwnd); err != nil {
				return 0
			}
			// 关键：必须在销毁窗口之前把输入内容读出来
			dlgResult = proxy{
				Name:          strings.TrimSpace(getText(dlgName)),
				Type:          dlgTypeNow(),
				LocalIP:       orDefault(strings.TrimSpace(getText(dlgLip)), "127.0.0.1"),
				LocalPort:     strings.TrimSpace(getText(dlgLport)),
				RemotePort:    strings.TrimSpace(getText(dlgRport)),
				CustomDomains: splitDomains(getText(dlgDomains)),
			}
			dlgResult.setDefaults()
			dlgOK = true
			pDestroyWindow.Call(uintptr(hwnd))
			return 0
		case didCancel, 2: // IDCANCEL
			pDestroyWindow.Call(uintptr(hwnd))
			return 0
		}
		return 0
	}
	if m == wmClose {
		pDestroyWindow.Call(uintptr(hwnd))
		return 0
	}
	switch m {
	case wmDrawItem:
		return drawOwnerItem(l)
	case wmEraseBkgnd:
		if r := eraseBackground(hwnd, syscall.Handle(w)); r != 0 {
			return r
		}
	case wmCtlColorBtn, wmCtlColorEdit, wmCtlColorListBox, wmCtlColorStatic:
		if br := ctlColor(m, syscall.Handle(w), l); br != 0 {
			return br
		}
	}
	r, _, _ := pDefWindowProcW.Call(uintptr(hwnd), uintptr(m), w, uintptr(l))
	return r
}

func validateEditFields(hwnd syscall.Handle) error {
	if strings.TrimSpace(getText(dlgName)) == "" {
		msgBoxOwner(hwnd, "代理名称不能为空", 0x10)
		return fmt.Errorf("empty name")
	}
	if _, err := validatePort(getText(dlgLport), "本地端口"); err != nil {
		msgBoxOwner(hwnd, err.Error(), 0x10)
		return err
	}
	t := dlgTypeNow()
	if t == "tcp" || t == "udp" {
		if _, err := validatePort(getText(dlgRport), "远程端口"); err != nil {
			msgBoxOwner(hwnd, err.Error(), 0x10)
			return err
		}
	} else {
		if len(splitDomains(getText(dlgDomains))) == 0 {
			msgBoxOwner(hwnd, t+" 代理需要填写自定义域名", 0x10)
			return fmt.Errorf("empty domains")
		}
	}
	return nil
}

func msgBoxOwner(owner syscall.Handle, body string, flags uintptr) {
	pMessageBoxW.Call(uintptr(owner), uintptr(unsafe.Pointer(u16(body))),
		uintptr(unsafe.Pointer(u16(appName))), flags)
}

var (
	dlgName, dlgLip, dlgLport, dlgRport                    syscall.Handle
	dlgTypeH, dlgDomains, dlgRportHint, dlgDomainsHint     syscall.Handle
	dlgDomainTip                                           syscall.Handle
	dlgOK                                                  bool
	// dlgResult 在窗口销毁【之前】捕获输入框内容。
	// 若等 pDestroyWindow 之后再 getText，控件已销毁，读回来全是空串。
	dlgResult proxy
)

// showEditDialog 模态显示代理编辑框；返回 nil 表示取消
func showEditDialog(p *proxy) *proxy {
	dlgOK = false
	dlgName, dlgLip, dlgLport, dlgRport = 0, 0, 0, 0

	dw, dh := ds(420), ds(280)
	mr, _ := getRect(hMain)
	x := int(mr.left) + (int(mr.right-mr.left)-dw)/2
	y := int(mr.top) + (int(mr.bottom-mr.top)-dh)/2

	style := uintptr(wsOverlapped | wsCaption | wsSysMenu | wsVisible)
	hw, _, _ := pCreateWindowExW.Call(0,
		uintptr(unsafe.Pointer(u16(editCls))),
		uintptr(unsafe.Pointer(u16("代理设置"))),
		style, uintptr(x), uintptr(y), uintptr(dw), uintptr(dh),
		uintptr(hMain), 0, uintptr(getInstance()), 0)
	hwnd := syscall.Handle(hw)
	if hwnd == 0 {
		return nil
	}

	mkc := func(class, text string, st uintptr, id, cx, cy, cw, ch int) syscall.Handle {
		r, _, _ := pCreateWindowExW.Call(0,
			uintptr(unsafe.Pointer(u16(class))), uintptr(unsafe.Pointer(u16(text))),
			st, uintptr(ds(cx)), uintptr(ds(cy)), uintptr(ds(cw)), uintptr(ds(ch)),
			uintptr(hwnd), uintptr(id), uintptr(getInstance()), 0)
		if r != 0 && hFont != 0 {
			pSendMessageW.Call(r, wmSetFont, hFont, 1)
		}
		return syscall.Handle(r)
	}

	es := uintptr(wsChild | wsVisible | wsTabStop | wsBorder | esAutoHScroll)

	// 类型下拉框
	mkc("STATIC", "类型", uintptr(wsChild|wsVisible), didTypeHint, 20, 22, 60, 22)
	dlgTypeH = mkc("COMBOBOX", "",
		uintptr(wsChild|wsVisible|wsTabStop|wsBorder|0x0003|0x0020|0x0200), // CBS_DROPDOWNLIST|CBS_AUTOHSCROLL|CBS_HASSTRINGS
		didType, 88, 19, 120, 200)
	for _, t := range proxyTypes {
		tp := u16(t)
		pSendMessageW.Call(uintptr(dlgTypeH), 0x0143 /*CB_ADDSTRING*/, 0, uintptr(unsafe.Pointer(tp)))
		runtime.KeepAlive(tp)
	}

	mkc("STATIC", "名称", uintptr(wsChild|wsVisible), 0, 230, 22, 40, 22)
	dlgName = mkc("EDIT", "", es, didName, 272, 19, 120, 24)

	mkc("STATIC", "本地 IP", uintptr(wsChild|wsVisible), 0, 20, 60, 60, 22)
	dlgLip = mkc("EDIT", "", es, didLip, 88, 57, 304, 24)

	mkc("STATIC", "本地端口", uintptr(wsChild|wsVisible), 0, 20, 98, 60, 22)
	dlgLport = mkc("EDIT", "", es, didLport, 88, 95, 130, 24)

	dlgRportHint = mkc("STATIC", "远程端口", uintptr(wsChild|wsVisible), didRportHint, 20, 136, 60, 22)
	dlgRport = mkc("EDIT", "", es, didRport, 88, 133, 130, 24)

	dlgDomainsHint = mkc("STATIC", "自定义域名", uintptr(wsChild|wsVisible), didDomainsHint, 20, 136, 68, 22)
	dlgDomains = mkc("EDIT", "", es, didDomains, 88, 133, 304, 24)

	dlgDomainTip = mkc("STATIC", "多个域名用逗号分隔",
		uintptr(wsChild|wsVisible), 0, 88, 162, 304, 20)

	mkc("BUTTON", "确定", uintptr(wsChild|wsVisible|wsTabStop|bsOwnerDraw), didOK, 200, 196, 84, 32)
	mkc("BUTTON", "取消", uintptr(wsChild|wsVisible|wsTabStop|bsOwnerDraw), didCancel, 300, 196, 84, 32)

	// 回填
	cur := &proxy{Type: "tcp", LocalIP: "127.0.0.1", LocalPort: "8080", RemotePort: "8080"}
	if p != nil {
		cur = p
	}
	cur.setDefaults()
	setText(dlgName, cur.Name)
	setText(dlgLip, cur.LocalIP)
	setText(dlgLport, cur.LocalPort)
	setText(dlgRport, cur.RemotePort)
	setText(dlgDomains, cur.domainText())
	// 选中类型
	for i, t := range proxyTypes {
		if t == cur.Type {
			pSendMessageW.Call(uintptr(dlgTypeH), 0x014E /*CB_SETCURSEL*/, uintptr(i), 0)
			break
		}
	}
	updateDlgEnabled()

	pEnableWindow.Call(uintptr(hMain), 0)
	pShowWindow.Call(uintptr(hwnd), swShow)
	pSetFocus.Call(uintptr(dlgName))

	// 模态等待：用 PeekMessage 轮询，保持主窗口消息继续被处理
	for isWindow(hwnd) {
		var m msg
		got, _, _ := pPeekMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0, 1)
		if got == 0 {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		if m.message == 0x0012 {
			break
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}

	pEnableWindow.Call(uintptr(hMain), 1)
	pSetForegroundWindow.Call(uintptr(hMain))

	if !dlgOK {
		return nil
	}
	res := dlgResult
	return &res
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func isWindow(h syscall.Handle) bool {
	r, _, _ := pIsWindow.Call(uintptr(h))
	return r != 0
}

func getRect(h syscall.Handle) (rect, error) {
	var r rect
	ok, _, err := pGetWindowRect.Call(uintptr(h), uintptr(unsafe.Pointer(&r)))
	if ok == 0 {
		return r, err
	}
	return r, nil
}

func mkFont(pt int) uintptr {
	h, _, _ := pCreateFontW.Call(
		uintptr(int32(-float64(pt)*96.0/72.0*dpiScale)), 0, 0, 0, 400, 0, 0, 0, 134, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(u16("Microsoft YaHei UI"))))
	return h
}

func main() {
	// Win32 的窗口、消息队列都绑定到创建它的线程上。
	// 必须把主 goroutine 锁在同一个 OS 线程，否则消息循环与窗口分属不同线程，
	// GetMessage 永远收不到消息（表现为窗口“未响应”）。
	runtime.LockOSThread()

	initDPI() // 必须在创建任何窗口/字体之前
	inst := getInstance()
	buildUI(inst)
	if hMain == 0 {
		return
	}

	c := loadCfg()
	setText(hAddr, c.ServerAddr)
	setText(hSport, c.ServerPort)
	setText(hToken, c.Token)

	// frpc.exe 路径：优先用上次保存的，否则自动探测
	fp := c.FrpcPath
	if fp == "" || checkFrpcFile(fp) != nil {
		fp = findFrpcAuto()
	}
	setText(hFrpc, fp)

	proxies = c.Proxies
	if len(proxies) == 0 {
		// 首次运行：给一条示例
		proxies = []*proxy{{Name: "test-tcp", Type: "tcp", LocalIP: "127.0.0.1", LocalPort: "22", RemotePort: "6000"}}
	}
	// 旧配置没有 type 字段，这里统一补齐，否则列表渲染不出「类型」
	for _, p := range proxies {
		p.setDefaults()
	}

	log("[就绪] " + appName)
	if fp == "" {
		log("[提示] 未找到 frpc.exe，请点“浏览…”选择它的位置。")
		log("[说明] frpc.exe 需自行下载：frp 官方 GitHub Releases（fatedier/frp）。")
	} else {
		log("[frpc] " + fp)
	}
	log("[说明] 填好服务器 IP 与 Token，用“添加”配置代理，然后“保存并启动”。")

	enableDragDrop(hMain) // 允许把 frpc.exe 拖进窗口
	pShowWindow.Call(uintptr(hMain), swShow)

	// 必须等窗口真正显示、ListView 完成创建后再填数据，
	// 否则子列文字会写不进去（只显示第一列）。
	pUpdateWindow.Call(uintptr(hMain))
	refreshList()

	// 应用上次保存的主题
	applyDarkMode(c.DarkMode)
	setCheck(hDarkChk, c.DarkMode)
	setCheck(hShowChk, showPwd.Load())

	// 用 PeekMessage 轮询，保证在无消息时也能跑队列刷新（否则界面会像卡死）
	var m msg
	for {
		got, _, _ := pPeekMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0, 1 /*PM_REMOVE*/)
		if got != 0 {
			if m.message == 0x0012 { // WM_QUIT
				break
			}
			pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
			pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
		} else {
			// 没有消息时做点空闲工作，避免 100% CPU
			pumpQueues()
			time.Sleep(10 * time.Millisecond)
		}
	}
}
