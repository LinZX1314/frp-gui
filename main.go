//go:build windows

// frpc-gui: FRP 客户端图形界面启动器（单文件 EXE，内嵌 frpc.exe）
//
// 构建: go build -ldflags "-H windowsgui -s -w" -o frpc-gui.exe
package main

import (
	_ "embed"
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

//go:embed frpc.exe
var frpcBin []byte

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

	colorWindow = 5

	idAddr, idSport, idToken = 1001, 1002, 1003

	idAdd, idEditBtn, idDelBtn, idUp, idDown = 2005, 2006, 2007, 2008, 2009
	idStart, idStop, idShow, idClear         = 2001, 2002, 2003, 2004
	idLog, idStatus, idProxy, idList         = 3001, 3002, 3003, 3004
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
)

func initCommonControls() {
	e := initCommonControlsEx{size: uint32(unsafe.Sizeof(initCommonControlsEx{})), icc: iccListView}
	pInitCommonCtrls.Call(uintptr(unsafe.Pointer(&e)))
}

// lvInsert 插入一行（4 列：名称 / 本地 / 远程 / 状态）
func lvInsert(idx int, cols [4]string) {
	// 这些 UTF-16 缓冲区必须存活到 SendMessage 返回之后
	keep := make([]*uint16, 4)
	for i := 0; i < 4; i++ {
		keep[i] = u16(cols[i])
	}
	defer runtime.KeepAlive(keep)

	// 一次调用写入全部列：LVITEM.iSubItem=0 插行，随后逐列 SetItemText。
	// 关键：LVITEM 的 a mask 必须是 LVIF_TEXT，且 pszText 指向的缓冲要一直有效。
	for c := 0; c < 4; c++ {
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
		lvInsert(i, [4]string{
			p.Name,
			p.LocalIP + ":" + p.LocalPort,
			":" + p.RemotePort,
			"待启动",
		})
	}
}

func setRowStatus(i int, s string) {
	if i < 0 || i >= len(proxies) {
		return
	}
	t := u16(s)
	it := lvItem{mask: lvifText, iItem: int32(i), iSubItem: 3, pszText: t}
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
	Proxies    []*proxy `json:"proxies"`
}

// proxy 一条 TCP 代理
type proxy struct {
	Name       string `json:"name"`
	LocalIP    string `json:"local_ip"`
	LocalPort  string `json:"local_port"`
	RemotePort string `json:"remote_port"`
}

func (p *proxy) setDefaults() {
	if strings.TrimSpace(p.Name) == "" {
		p.Name = "tcp"
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
	hAddr, hSport, hToken                          syscall.Handle
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
		style, uintptr(x), uintptr(y), uintptr(w), uintptr(h),
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
		style, uintptr(x), uintptr(y), uintptr(w), uintptr(h),
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

// extractFrpc 把内嵌的 frpc.exe 释放到程序自己的运行目录
func extractFrpc() (string, error) {
	dir := W()
	p := filepath.Join(dir, "frpc.exe")
	if st, err := os.Stat(p); err == nil && st.Size() == int64(len(frpcBin)) {
		return p, nil
	}
	if err := os.WriteFile(p, frpcBin, 0o755); err != nil {
		return "", err
	}
	return p, nil
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
		b.WriteString("type = \"tcp\"\r\n")
		fmt.Fprintf(&b, "localIP = \"%s\"\r\n", esc(p.LocalIP))
		fmt.Fprintf(&b, "localPort = %s\r\n", p.LocalPort)
		fmt.Fprintf(&b, "remotePort = %s\r\n", p.RemotePort)
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
		Proxies:    proxies,
	}
	if c.ServerAddr == "" {
		return nil, fmt.Errorf("服务器 IP 不能为空")
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
		if p.RemotePort, err = validatePort(p.RemotePort, fmt.Sprintf("第 %d 条代理的远程端口", no)); err != nil {
			return nil, err
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

	exe, err := extractFrpc()
	if err != nil {
		msgBox("释放 frpc.exe 失败：\r\n"+err.Error(), 0x10)
		return
	}

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
}

func wndProc(hwnd syscall.Handle, m uint32, w, l uintptr) uintptr {
	switch m {
	case wmCommand:
		switch int(w & 0xffff) {
		case idStart:
			go startFrpc()
		case idStop:
			go stopFrpc()
		case idShow:
			toggleToken()
		case idClear:
			setText(hLog, "")
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
	r, _, _ := pDefWindowProcW.Call(uintptr(hwnd), uintptr(m), w, l)
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
		style, 60, 60, 700, 660, 0, 0, uintptr(inst), 0)
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
	mk("BUTTON", "显示", uintptr(wsChild|wsVisible|wsTabStop|bsAutoCheck), idShow, 430, 70, 70, 24)

	// ---- 代理列表 ----
	mk("BUTTON", "代理列表（可添加多条 TCP）", uintptr(wsChild|wsVisible|bsGroupBox), 0, 14, 122, 660, 190)

	hList = mkNoFont("SysListView32", "", uintptr(wsChild|wsVisible|wsBorder|wsTabStop|lvsReport|lvsShowSel), idList, 28, 148, 632, 120)
	pSendMessageW.Call(uintptr(hList), lvmSetExtStyle, uintptr(lvsExFullRow|lvsExGrid), uintptr(lvsExFullRow|lvsExGrid))
	addColumn(0, "名称", 150)
	addColumn(1, "本地地址", 200)
	addColumn(2, "远程端口", 110)
	addColumn(3, "状态", 150)
	// 列表内容在窗口显示后填充（见 main），此处不填

	mk("BUTTON", "添加", uintptr(wsChild|wsVisible|wsTabStop|bsPushButton), idAdd, 28, 274, 80, 30)
	mk("BUTTON", "修改", uintptr(wsChild|wsVisible|wsTabStop|bsPushButton), idEditBtn, 114, 274, 80, 30)
	mk("BUTTON", "删除", uintptr(wsChild|wsVisible|wsTabStop|bsPushButton), idDelBtn, 200, 274, 80, 30)
	mk("BUTTON", "上移", uintptr(wsChild|wsVisible|wsTabStop|bsPushButton), idUp, 286, 274, 70, 30)
	mk("BUTTON", "下移", uintptr(wsChild|wsVisible|wsTabStop|bsPushButton), idDown, 362, 274, 70, 30)

	// ---- 按钮 ----
	hStart = mk("BUTTON", "保存并启动", uintptr(wsChild|wsVisible|wsTabStop|bsPushButton), idStart, 18, 320, 120, 34)
	hStop = mk("BUTTON", "停止", uintptr(wsChild|wsVisible|wsTabStop|bsPushButton), idStop, 146, 320, 88, 34)
	pEnableWindow.Call(uintptr(hStop), 0)
	mk("BUTTON", "清空日志", uintptr(wsChild|wsVisible|wsTabStop|bsPushButton), idClear, 242, 320, 92, 34)
	hStatus = mk("STATIC", "● 未运行", uintptr(wsChild|wsVisible), idStatus, 570, 330, 100, 22)
	hProxyInfo = mk("STATIC", "", uintptr(wsChild|wsVisible), idProxy, 346, 330, 216, 22)

	// ---- 日志 ----
	mk("STATIC", "日志", uintptr(wsChild|wsVisible), 0, 18, 362, 60, 20)
	hLog = mk("EDIT", "",
		uintptr(wsChild|wsVisible|wsBorder|wsVScroll|esLeft|esMultiline|esAutoHScroll|esReadOnly),
		idLog, 18, 384, 660, 240)

	setStatus("● 未运行", false)
}

func addColumn(i int, title string, w int) {
	// LVCF_FMT(0x1) | LVCF_WIDTH(0x2) | LVCF_TEXT(0x4) | LVCF_SUBITEM(0x8) = 0xF
	t := u16(title)
	c := lvColumn{mask: 0x000F, fmt: lvcfmtLeft, cx: int32(w), pszText: t, iSubItem: int32(i)}
	pSendMessageW.Call(uintptr(hList), lvmInsertColW, uintptr(i), uintptr(unsafe.Pointer(&c)))
	runtime.KeepAlive(t)   // 保证 title 的 UTF-16 缓冲在调用期间存活
	runtime.KeepAlive(c)
}

// ---------------- 代理编辑对话框 ----------------

const (
	didOK, didCancel                    = 4001, 4002
	didName, didLip, didLport, didRport = 4003, 4004, 4005, 4006
)

var editCls = "FrpcEditDlg"

func editProc(hwnd syscall.Handle, m uint32, w, l uintptr) uintptr {
	if m == wmCommand {
		switch int(w & 0xffff) {
		case didOK:
			if err := validateEditFields(hwnd); err != nil {
				return 0
			}
			// 关键：必须在销毁窗口之前把输入内容读出来
			dlgResult = proxy{
				Name:       strings.TrimSpace(getText(dlgName)),
				LocalIP:    orDefault(strings.TrimSpace(getText(dlgLip)), "127.0.0.1"),
				LocalPort:  strings.TrimSpace(getText(dlgLport)),
				RemotePort: strings.TrimSpace(getText(dlgRport)),
			}
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
	r, _, _ := pDefWindowProcW.Call(uintptr(hwnd), uintptr(m), w, l)
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
	if _, err := validatePort(getText(dlgRport), "远程端口"); err != nil {
		msgBoxOwner(hwnd, err.Error(), 0x10)
		return err
	}
	return nil
}

func msgBoxOwner(owner syscall.Handle, body string, flags uintptr) {
	pMessageBoxW.Call(uintptr(owner), uintptr(unsafe.Pointer(u16(body))),
		uintptr(unsafe.Pointer(u16(appName))), flags)
}

var (
	dlgName, dlgLip, dlgLport, dlgRport syscall.Handle
	dlgOK                               bool
	// dlgResult 在窗口销毁【之前】捕获输入框内容。
	// 若等 pDestroyWindow 之后再 getText，控件已销毁，读回来全是空串。
	dlgResult proxy
)

// showEditDialog 模态显示代理编辑框；返回 nil 表示取消
func showEditDialog(p *proxy) *proxy {
	dlgOK = false
	dlgName, dlgLip, dlgLport, dlgRport = 0, 0, 0, 0

	dw, dh := 400, 246
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
			st, uintptr(cx), uintptr(cy), uintptr(cw), uintptr(ch),
			uintptr(hwnd), uintptr(id), uintptr(getInstance()), 0)
		if r != 0 && hFont != 0 {
			pSendMessageW.Call(r, wmSetFont, hFont, 1)
		}
		return syscall.Handle(r)
	}

	es := uintptr(wsChild | wsVisible | wsTabStop | wsBorder | esAutoHScroll)
	mkc("STATIC", "名称", uintptr(wsChild|wsVisible), 0, 20, 22, 60, 22)
	dlgName = mkc("EDIT", "", es, didName, 88, 18, 270, 24)
	mkc("STATIC", "本地 IP", uintptr(wsChild|wsVisible), 0, 20, 60, 60, 22)
	dlgLip = mkc("EDIT", "", es, didLip, 88, 56, 270, 24)
	mkc("STATIC", "本地端口", uintptr(wsChild|wsVisible), 0, 20, 98, 60, 22)
	dlgLport = mkc("EDIT", "", es, didLport, 88, 94, 270, 24)
	mkc("STATIC", "远程端口", uintptr(wsChild|wsVisible), 0, 20, 136, 60, 22)
	dlgRport = mkc("EDIT", "", es, didRport, 88, 132, 270, 24)
	mkc("BUTTON", "确定", uintptr(wsChild|wsVisible|wsTabStop|bsPushButton|0x00000001), didOK, 176, 174, 84, 32)
	mkc("BUTTON", "取消", uintptr(wsChild|wsVisible|wsTabStop|bsPushButton), didCancel, 268, 174, 84, 32)

	if p != nil {
		setText(dlgName, p.Name)
		setText(dlgLip, p.LocalIP)
		setText(dlgLport, p.LocalPort)
		setText(dlgRport, p.RemotePort)
	} else {
		// 新增时给出可用默认值，避免用户面对一串空框
		setText(dlgLip, "127.0.0.1")
		setText(dlgLport, "8080")
		setText(dlgRport, "8080")
	}

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
		uintptr(int32(-pt*96/72)), 0, 0, 0, 400, 0, 0, 0, 134, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(u16("Microsoft YaHei UI"))))
	return h
}

func main() {
	// Win32 的窗口、消息队列都绑定到创建它的线程上。
	// 必须把主 goroutine 锁在同一个 OS 线程，否则消息循环与窗口分属不同线程，
	// GetMessage 永远收不到消息（表现为窗口“未响应”）。
	runtime.LockOSThread()

	inst := getInstance()
	buildUI(inst)
	if hMain == 0 {
		return
	}

	c := loadCfg()
	setText(hAddr, c.ServerAddr)
	setText(hSport, c.ServerPort)
	setText(hToken, c.Token)

	proxies = c.Proxies
	if len(proxies) == 0 {
		// 首次运行：给一条示例
		proxies = []*proxy{{Name: "test-tcp", LocalIP: "127.0.0.1", LocalPort: "22", RemotePort: "6000"}}
	}

	log("[就绪] " + appName)
	log("[说明] 填写服务器 IP 与 Token，用“添加”增加多条代理，然后“保存并启动”。")
	log("[说明] frpc.exe 已内嵌，无需外部文件。")

	pShowWindow.Call(uintptr(hMain), swShow)

	// 必须等窗口真正显示、ListView 完成创建后再填数据，
	// 否则子列文字会写不进去（只显示第一列）。
	pUpdateWindow.Call(uintptr(hMain))
	refreshList()

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
