//go:build windows

// gotool — 즐겨찾기(바로가기) 런처.
// 실행하면 마우스 커서 위치에 우클릭 메뉴 모양의 3열 팝업(웹 즐겨찾기 | 앱 | 폴더)이 뜨고,
// 항목을 클릭하면 실행, 우클릭하면 삭제할 수 있다.
//
// 사용법:
//
//	gotool.exe              exe 옆 shortcuts 폴더의 내용을 3열 메뉴로 표시
//	gotool.exe <폴더>       지정한 폴더의 내용을 표시
//	gotool.exe add <경로>   파일/폴더를 shortcuts 폴더에 추가 (.lnk/.url은 복사, 그 외는 바로가기 생성)
//	gotool.exe install      탐색기 우클릭 메뉴에 "gotool에 추가" 등록
//	gotool.exe uninstall    우클릭 메뉴 등록 해제
package main

import (
	"fmt"
	neturl "net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Win32 창/메뉴는 만든 스레드에서만 다룰 수 있으므로 main 고루틴을 OS 스레드에 고정한다.
func init() {
	runtime.LockOSThread()
}

var (
	user32  = windows.NewLazySystemDLL("user32.dll")
	gdi32   = windows.NewLazySystemDLL("gdi32.dll")
	shell32 = windows.NewLazySystemDLL("shell32.dll")
	ole32   = windows.NewLazySystemDLL("ole32.dll")
	kernel  = windows.NewLazySystemDLL("kernel32.dll")

	procRegisterClassExW    = user32.NewProc("RegisterClassExW")
	procCreateWindowExW     = user32.NewProc("CreateWindowExW")
	procDefWindowProcW      = user32.NewProc("DefWindowProcW")
	procDestroyWindow       = user32.NewProc("DestroyWindow")
	procCreatePopupMenu     = user32.NewProc("CreatePopupMenu")
	procDestroyMenu         = user32.NewProc("DestroyMenu")
	procInsertMenuItemW     = user32.NewProc("InsertMenuItemW")
	procGetMenuItemID       = user32.NewProc("GetMenuItemID")
	procEndMenu             = user32.NewProc("EndMenu")
	procTrackPopupMenuEx    = user32.NewProc("TrackPopupMenuEx")
	procGetCursorPos        = user32.NewProc("GetCursorPos")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procMessageBoxW         = user32.NewProc("MessageBoxW")
	procGetSystemMetrics    = user32.NewProc("GetSystemMetrics")
	procSetProcessDPIAware  = user32.NewProc("SetProcessDPIAware")
	procDrawIconEx          = user32.NewProc("DrawIconEx")
	procDestroyIcon         = user32.NewProc("DestroyIcon")
	procGetDC               = user32.NewProc("GetDC")
	procReleaseDC           = user32.NewProc("ReleaseDC")

	procCreateCompatibleDC = gdi32.NewProc("CreateCompatibleDC")
	procDeleteDC           = gdi32.NewProc("DeleteDC")
	procCreateDIBSection   = gdi32.NewProc("CreateDIBSection")
	procSelectObject       = gdi32.NewProc("SelectObject")
	procDeleteObject       = gdi32.NewProc("DeleteObject")

	procSHGetFileInfoW = shell32.NewProc("SHGetFileInfoW")
	procShellExecuteW  = shell32.NewProc("ShellExecuteW")

	procCoInitializeEx   = ole32.NewProc("CoInitializeEx")
	procCoUninitialize   = ole32.NewProc("CoUninitialize")
	procCoCreateInstance = ole32.NewProc("CoCreateInstance")

	procGetModuleHandleW = kernel.NewProc("GetModuleHandleW")
)

const (
	wsPopup = 0x80000000

	miimState       = 0x0001
	miimID          = 0x0002
	miimString      = 0x0040
	miimBitmap      = 0x0080
	miimFType       = 0x0100
	mftMenuBarBreak = 0x0020
	mftSeparator    = 0x0800
	mfsGrayed       = 0x0003

	tpmReturnCmd = 0x0100

	wmMenuRButtonUp = 0x0122

	shgfiIcon      = 0x00000100
	shgfiSmallIcon = 0x00000001

	smCxSmIcon = 49
	smCySmIcon = 50

	diNormal     = 0x0003
	dibRGBColors = 0

	swShowNormal = 1

	mbOK              = 0x00000000
	mbYesNo           = 0x00000004
	mbIconError       = 0x00000010
	mbIconQuestion    = 0x00000020
	mbIconWarning     = 0x00000030
	mbIconInformation = 0x00000040
	idYes             = 6

	clsctxInprocServer = 1
	coinitApartment    = 2
)

// 카테고리(=메뉴 열)
const (
	catWeb = iota
	catApp
	catFolder
	catCount
)

var catNames = [catCount]string{"🌐 웹 즐겨찾기", "🚀 앱", "📁 폴더"}

// 관리용 메뉴 ID (일반 항목은 1부터 순차 할당)
const (
	idOpenFolder = 0xFFF0
	idInstall    = 0xFFF1
)

type point struct{ x, y int32 }

type wndClassEx struct {
	cbSize        uint32
	style         uint32
	lpfnWndProc   uintptr
	cbClsExtra    int32
	cbWndExtra    int32
	hInstance     windows.Handle
	hIcon         windows.Handle
	hCursor       windows.Handle
	hbrBackground windows.Handle
	lpszMenuName  *uint16
	lpszClassName *uint16
	hIconSm       windows.Handle
}

type menuItemInfo struct {
	cbSize        uint32
	fMask         uint32
	fType         uint32
	fState        uint32
	wID           uint32
	hSubMenu      windows.Handle
	hbmpChecked   windows.Handle
	hbmpUnchecked windows.Handle
	dwItemData    uintptr
	dwTypeData    *uint16
	cch           uint32
	hbmpItem      windows.Handle
}

type shFileInfo struct {
	hIcon         windows.Handle
	iIcon         int32
	dwAttributes  uint32
	szDisplayName [260]uint16
	szTypeName    [80]uint16
}

type bitmapInfoHeader struct {
	biSize          uint32
	biWidth         int32
	biHeight        int32
	biPlanes        uint16
	biBitCount      uint16
	biCompression   uint32
	biSizeImage     uint32
	biXPelsPerMeter int32
	biYPelsPerMeter int32
	biClrUsed       uint32
	biClrImportant  uint32
}

type bitmapInfo struct {
	header bitmapInfoHeader
	colors [1]uint32
}

type item struct {
	label   string
	path    string // ShellExecute 대상(바로가기 파일 자체)
	iconSrc string // 아이콘을 가져올 경로
}

type app struct {
	dataDir string
	nextID  uint32
	cmds    map[uint32]string // 메뉴 ID → 실행할 경로
	files   map[uint32]string // 메뉴 ID → 삭제 가능한 파일 경로
	bitmaps []windows.Handle
	iconCx  int32
	iconCy  int32
	rbPath  string // 항목 우클릭으로 삭제 요청된 경로
}

func main() {
	// windowsgui 빌드는 콘솔이 없어 패닉이 보이지 않으므로 메시지박스로 알린다.
	defer func() {
		if r := recover(); r != nil {
			alert("gotool", fmt.Sprintf("오류가 발생했습니다:\n%v", r), mbIconError)
		}
	}()

	procSetProcessDPIAware.Call()
	procCoInitializeEx.Call(0, coinitApartment)
	defer procCoUninitialize.Call()

	args := os.Args[1:]
	if len(args) > 0 {
		switch strings.ToLower(args[0]) {
		case "add":
			if len(args) < 2 {
				alert("gotool", "사용법: gotool.exe add <파일 또는 폴더 경로>", mbIconInformation)
				return
			}
			addItem(args[1])
			return
		case "install":
			if err := installContextMenu(); err != nil {
				alert("gotool", "우클릭 메뉴 등록 실패:\n"+err.Error(), mbIconError)
			} else {
				alert("gotool", "탐색기 우클릭 메뉴에 \"gotool에 추가\"를 등록했습니다.\n\nWindows 11에서는 우클릭 후 \"더 많은 옵션 표시\" 안에 나타납니다.", mbIconInformation)
			}
			return
		case "uninstall":
			if err := uninstallContextMenu(); err != nil {
				alert("gotool", "우클릭 메뉴 해제 실패:\n"+err.Error(), mbIconError)
			} else {
				alert("gotool", "우클릭 메뉴 등록을 해제했습니다.", mbIconInformation)
			}
			return
		}
	}

	dir, err := resolveDir(args)
	if err != nil {
		alert("gotool", err.Error(), mbIconError)
		return
	}
	runMenu(dir)
}

// resolveDir 는 표시할 폴더를 정한다. 인자로 폴더를 받으면 그 폴더,
// 아니면 exe 옆 shortcuts 폴더(없으면 자동 생성).
func resolveDir(args []string) (string, error) {
	if len(args) > 0 {
		dir := args[0]
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			return dir, nil
		}
		return "", fmt.Errorf("폴더를 찾을 수 없습니다:\n%s", args[0])
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(filepath.Dir(exe), "shortcuts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("shortcuts 폴더를 만들 수 없습니다:\n%s\n%v", dir, err)
	}
	return dir, nil
}

func runMenu(dir string) {
	a := &app{
		dataDir: dir,
		iconCx:  getSystemMetrics(smCxSmIcon),
		iconCy:  getSystemMetrics(smCySmIcon),
	}

	hwnd := createHiddenWindow(a)
	if hwnd == 0 {
		alert("gotool", "창을 생성하지 못했습니다.", mbIconError)
		return
	}
	defer procDestroyWindow.Call(uintptr(hwnd))

	for {
		a.nextID = 1
		a.cmds = map[uint32]string{}
		a.files = map[uint32]string{}
		a.rbPath = ""

		menu := a.buildMenu()

		var pt point
		procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
		procSetForegroundWindow.Call(uintptr(hwnd))

		cmd, _, callErr := procTrackPopupMenuEx.Call(
			uintptr(menu),
			tpmReturnCmd,
			uintptr(pt.x), uintptr(pt.y),
			uintptr(hwnd), 0,
		)

		procDestroyMenu.Call(uintptr(menu))
		for _, b := range a.bitmaps {
			procDeleteObject.Call(uintptr(b))
		}
		a.bitmaps = nil

		switch {
		case cmd == uintptr(idOpenFolder):
			launch(a.dataDir)
			return
		case cmd == uintptr(idInstall):
			if err := installContextMenu(); err != nil {
				alert("gotool", "우클릭 메뉴 등록 실패:\n"+err.Error(), mbIconError)
			} else {
				alert("gotool", "탐색기 우클릭 메뉴에 \"gotool에 추가\"를 등록했습니다.\n\nWindows 11에서는 우클릭 후 \"더 많은 옵션 표시\" 안에 나타납니다.", mbIconInformation)
			}
			continue // 메뉴 다시 표시
		case cmd != 0:
			if path, ok := a.cmds[uint32(cmd)]; ok {
				launch(path)
			}
			return
		case a.rbPath != "":
			a.confirmDelete(a.rbPath)
			continue // 삭제 후 메뉴 다시 표시
		default:
			// 취소. 실제 실패라면 원인을 표시한다.
			if errno, ok := callErr.(syscall.Errno); ok && errno != 0 {
				alert("gotool", fmt.Sprintf("메뉴를 표시하지 못했습니다.\n(오류 %d: %v)", uint32(errno), errno), mbIconError)
			}
			return
		}
	}
}

// confirmDelete 는 확인 후 바로가기를 삭제한다. 안전을 위해 데이터 폴더 안의 항목만 지운다.
func (a *app) confirmDelete(path string) {
	rel, err := filepath.Rel(a.dataDir, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return
	}
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	r, _, _ := procMessageBoxW.Call(0,
		uintptr(unsafe.Pointer(utf16Ptr(fmt.Sprintf("\"%s\" 을(를) 삭제할까요?\n\n%s", name, path)))),
		uintptr(unsafe.Pointer(utf16Ptr("gotool - 바로가기 삭제"))),
		mbYesNo|mbIconWarning)
	if r != idYes {
		return
	}
	if err := os.RemoveAll(path); err != nil {
		alert("gotool", "삭제하지 못했습니다:\n"+err.Error(), mbIconError)
	}
}

// ---- 메뉴 구성 ----

func (a *app) buildMenu() windows.Handle {
	hMenu, _, _ := procCreatePopupMenu.Call()
	menu := windows.Handle(hMenu)

	cats := a.scan()
	var pos uint32
	for ci := 0; ci < catCount; ci++ {
		a.insertHeader(menu, &pos, catNames[ci], ci > 0)
		if len(cats[ci]) == 0 {
			a.insertDisabled(menu, &pos, "    (비어 있음)")
		}
		for _, it := range cats[ci] {
			id := a.nextID
			a.nextID++
			a.cmds[id] = it.path
			a.files[id] = it.path
			a.insertItem(menu, &pos, id, it.label, a.iconBitmap(it.iconSrc))
		}
	}

	// 마지막 열 하단: 관리 항목
	a.insertSeparator(menu, &pos)
	a.insertItem(menu, &pos, idOpenFolder, "⚙ 바로가기 폴더 열기 (추가·수정·삭제)", 0)
	if !contextMenuInstalled() {
		a.insertItem(menu, &pos, idInstall, "🖱 우클릭 \"gotool에 추가\" 메뉴 등록", 0)
	}
	return menu
}

// scan 은 데이터 폴더의 항목을 읽어 3개 카테고리로 분류한다.
func (a *app) scan() [catCount][]item {
	var cats [catCount][]item
	entries, err := os.ReadDir(a.dataDir)
	if err != nil {
		return cats
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || strings.EqualFold(name, "desktop.ini") {
			continue
		}
		full := filepath.Join(a.dataDir, name)
		cat, iconSrc := classify(full, e.IsDir())
		label := name
		if !e.IsDir() {
			label = strings.TrimSuffix(name, filepath.Ext(name))
		}
		cats[cat] = append(cats[cat], item{label: label, path: full, iconSrc: iconSrc})
	}
	for ci := range cats {
		sort.Slice(cats[ci], func(i, j int) bool {
			return strings.ToLower(cats[ci][i].label) < strings.ToLower(cats[ci][j].label)
		})
	}
	return cats
}

// classify 는 항목의 카테고리(열)와 아이콘 소스 경로를 정한다.
//   - 실제 폴더            → 폴더
//   - .url  http/https     → 웹
//   - .url  file://폴더    → 폴더, file://파일 → 앱
//   - .lnk  대상이 폴더    → 폴더, 그 외 → 앱
//   - 기타 파일            → 앱
func classify(full string, isDir bool) (int, string) {
	if isDir {
		return catFolder, full
	}
	switch strings.ToLower(filepath.Ext(full)) {
	case ".url":
		target := urlTarget(full)
		lower := strings.ToLower(target)
		if strings.HasPrefix(lower, "file:") {
			if local := fileURLToPath(target); local != "" {
				if st, err := os.Stat(local); err == nil {
					if st.IsDir() {
						return catFolder, local
					}
					return catApp, local
				}
			}
			return catApp, full
		}
		return catWeb, full
	case ".lnk":
		if t := lnkTarget(full); t != "" {
			if st, err := os.Stat(t); err == nil && st.IsDir() {
				return catFolder, full
			}
		}
		return catApp, full
	default:
		return catApp, full
	}
}

// urlTarget 은 .url(인터넷 바로가기) 파일에서 URL= 값을 읽는다.
func urlTarget(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if len(line) > 4 && strings.EqualFold(line[:4], "URL=") {
			return strings.TrimSpace(line[4:])
		}
	}
	return ""
}

func fileURLToPath(raw string) string {
	u, err := neturl.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(u.Scheme, "file") {
		return ""
	}
	if u.Host != "" { // UNC: file://server/share/...
		return `\\` + u.Host + filepath.FromSlash(u.Path)
	}
	return filepath.FromSlash(strings.TrimPrefix(u.Path, "/"))
}

func (a *app) insertHeader(menu windows.Handle, pos *uint32, text string, columnBreak bool) {
	mii := menuItemInfo{
		cbSize:     uint32(unsafe.Sizeof(menuItemInfo{})),
		fMask:      miimString | miimState | miimFType,
		fState:     mfsGrayed,
		dwTypeData: utf16Ptr(text),
	}
	if columnBreak {
		mii.fType = mftMenuBarBreak
	}
	procInsertMenuItemW.Call(uintptr(menu), uintptr(*pos), 1, uintptr(unsafe.Pointer(&mii)))
	*pos++
}

func (a *app) insertDisabled(menu windows.Handle, pos *uint32, text string) {
	mii := menuItemInfo{
		cbSize:     uint32(unsafe.Sizeof(menuItemInfo{})),
		fMask:      miimString | miimState,
		fState:     mfsGrayed,
		dwTypeData: utf16Ptr(text),
	}
	procInsertMenuItemW.Call(uintptr(menu), uintptr(*pos), 1, uintptr(unsafe.Pointer(&mii)))
	*pos++
}

func (a *app) insertSeparator(menu windows.Handle, pos *uint32) {
	mii := menuItemInfo{
		cbSize: uint32(unsafe.Sizeof(menuItemInfo{})),
		fMask:  miimFType,
		fType:  mftSeparator,
	}
	procInsertMenuItemW.Call(uintptr(menu), uintptr(*pos), 1, uintptr(unsafe.Pointer(&mii)))
	*pos++
}

func (a *app) insertItem(menu windows.Handle, pos *uint32, id uint32, label string, bmp windows.Handle) {
	mii := menuItemInfo{
		cbSize:     uint32(unsafe.Sizeof(menuItemInfo{})),
		fMask:      miimString | miimID,
		wID:        id,
		dwTypeData: utf16Ptr(strings.ReplaceAll(label, "&", "&&")),
	}
	if bmp != 0 {
		mii.fMask |= miimBitmap
		mii.hbmpItem = bmp
	}
	procInsertMenuItemW.Call(uintptr(menu), uintptr(*pos), 1, uintptr(unsafe.Pointer(&mii)))
	*pos++
}

// ---- 아이콘 ----

// iconBitmap 은 경로의 셸 아이콘을 메뉴용 32bpp HBITMAP으로 만든다.
func (a *app) iconBitmap(path string) windows.Handle {
	if path == "" {
		return 0
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0
	}
	var sfi shFileInfo
	r, _, _ := procSHGetFileInfoW.Call(
		uintptr(unsafe.Pointer(p)), 0,
		uintptr(unsafe.Pointer(&sfi)), unsafe.Sizeof(sfi),
		shgfiIcon|shgfiSmallIcon,
	)
	if r == 0 || sfi.hIcon == 0 {
		return 0
	}
	defer procDestroyIcon.Call(uintptr(sfi.hIcon))

	bmp := a.bitmapFromIcon(sfi.hIcon)
	if bmp != 0 {
		a.bitmaps = append(a.bitmaps, bmp)
	}
	return bmp
}

func (a *app) bitmapFromIcon(icon windows.Handle) windows.Handle {
	hdc, _, _ := procGetDC.Call(0)
	if hdc == 0 {
		return 0
	}
	defer procReleaseDC.Call(0, hdc)

	memDC, _, _ := procCreateCompatibleDC.Call(hdc)
	if memDC == 0 {
		return 0
	}
	defer procDeleteDC.Call(memDC)

	bi := bitmapInfo{header: bitmapInfoHeader{
		biSize:     uint32(unsafe.Sizeof(bitmapInfoHeader{})),
		biWidth:    a.iconCx,
		biHeight:   -a.iconCy, // top-down
		biPlanes:   1,
		biBitCount: 32,
	}}
	var bits uintptr
	hbmp, _, _ := procCreateDIBSection.Call(hdc, uintptr(unsafe.Pointer(&bi)), dibRGBColors, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if hbmp == 0 {
		return 0
	}

	old, _, _ := procSelectObject.Call(memDC, hbmp)
	procDrawIconEx.Call(memDC, 0, 0, uintptr(icon), uintptr(a.iconCx), uintptr(a.iconCy), 0, 0, diNormal)
	procSelectObject.Call(memDC, old)
	return windows.Handle(hbmp)
}

// ---- 추가(add) ----

// addItem 은 경로를 shortcuts 폴더에 추가한다.
// .lnk/.url 은 그대로 복사하고, 그 외 파일/폴더는 .lnk 바로가기를 만든다.
func addItem(src string) {
	src = strings.Trim(src, `"`)
	abs, err := filepath.Abs(src)
	if err == nil {
		src = abs
	}
	st, err := os.Stat(src)
	if err != nil {
		alert("gotool", "경로를 찾을 수 없습니다:\n"+src, mbIconError)
		return
	}

	dir, err := resolveDir(nil)
	if err != nil {
		alert("gotool", err.Error(), mbIconError)
		return
	}

	base := filepath.Base(src)
	ext := strings.ToLower(filepath.Ext(base))
	var dest string
	if !st.IsDir() && (ext == ".lnk" || ext == ".url") {
		dest = uniqueDest(dir, base)
		if err := copyFile(src, dest); err != nil {
			alert("gotool", "복사하지 못했습니다:\n"+err.Error(), mbIconError)
			return
		}
	} else {
		name := base
		if !st.IsDir() {
			name = strings.TrimSuffix(base, filepath.Ext(base))
		}
		dest = uniqueDest(dir, name+".lnk")
		if err := createLnk(src, dest); err != nil {
			alert("gotool", "바로가기를 만들지 못했습니다:\n"+err.Error(), mbIconError)
			return
		}
	}

	cat, _ := classify(dest, false)
	label := strings.TrimSuffix(filepath.Base(dest), filepath.Ext(dest))
	alert("gotool", fmt.Sprintf("추가되었습니다: %s\n분류: %s", label, catNames[cat]), mbIconInformation)
}

func uniqueDest(dir, name string) string {
	dest := filepath.Join(dir, name)
	if _, err := os.Stat(dest); os.IsNotExist(err) {
		return dest
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		dest = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, i, ext))
		if _, err := os.Stat(dest); os.IsNotExist(err) {
			return dest
		}
	}
}

func copyFile(src, dest string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dest, data, 0o644)
}

// ---- 탐색기 우클릭 메뉴 등록 ----

var contextMenuKeys = []string{
	`Software\Classes\*\shell\gotool.add`,
	`Software\Classes\Directory\shell\gotool.add`,
}

func installContextMenu() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	for _, base := range contextMenuKeys {
		k, _, err := registry.CreateKey(registry.CURRENT_USER, base, registry.SET_VALUE)
		if err != nil {
			return err
		}
		k.SetStringValue("", "gotool에 추가")
		k.SetStringValue("Icon", exe)
		k.Close()

		c, _, err := registry.CreateKey(registry.CURRENT_USER, base+`\command`, registry.SET_VALUE)
		if err != nil {
			return err
		}
		c.SetStringValue("", `"`+exe+`" add "%1"`)
		c.Close()
	}
	return nil
}

func uninstallContextMenu() error {
	var firstErr error
	for _, base := range contextMenuKeys {
		if err := registry.DeleteKey(registry.CURRENT_USER, base+`\command`); err != nil && firstErr == nil && err != registry.ErrNotExist {
			firstErr = err
		}
		if err := registry.DeleteKey(registry.CURRENT_USER, base); err != nil && firstErr == nil && err != registry.ErrNotExist {
			firstErr = err
		}
	}
	return firstErr
}

func contextMenuInstalled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, contextMenuKeys[0], registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	k.Close()
	return true
}

// ---- .lnk 읽기/생성 (IShellLinkW COM) ----

var (
	clsidShellLink  = windows.GUID{Data1: 0x00021401, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidIShellLinkW  = windows.GUID{Data1: 0x000214F9, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
	iidIPersistFile = windows.GUID{Data1: 0x0000010B, Data4: [8]byte{0xC0, 0, 0, 0, 0, 0, 0, 0x46}}
)

// IShellLinkW vtable: 0 QueryInterface, 1 AddRef, 2 Release, 3 GetPath, ...,
// 9 SetWorkingDirectory, ..., 20 SetPath
// IPersistFile vtable: ..., 5 Load, 6 Save
func comCall(obj unsafe.Pointer, idx uintptr, args ...uintptr) uintptr {
	vtbl := *(**[32]uintptr)(obj)
	r, _, _ := syscall.SyscallN(vtbl[idx], append([]uintptr{uintptr(obj)}, args...)...)
	return r
}

func newShellLink() (psl, ppf unsafe.Pointer, ok bool) {
	hr, _, _ := procCoCreateInstance.Call(
		uintptr(unsafe.Pointer(&clsidShellLink)), 0, clsctxInprocServer,
		uintptr(unsafe.Pointer(&iidIShellLinkW)), uintptr(unsafe.Pointer(&psl)))
	if hr != 0 || psl == nil {
		return nil, nil, false
	}
	if comCall(psl, 0, uintptr(unsafe.Pointer(&iidIPersistFile)), uintptr(unsafe.Pointer(&ppf))) != 0 || ppf == nil {
		comCall(psl, 2) // Release
		return nil, nil, false
	}
	return psl, ppf, true
}

// lnkTarget 은 .lnk 바로가기의 대상 경로를 돌려준다(실패 시 "").
func lnkTarget(path string) string {
	psl, ppf, ok := newShellLink()
	if !ok {
		return ""
	}
	defer comCall(psl, 2)
	defer comCall(ppf, 2)

	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return ""
	}
	if comCall(ppf, 5, uintptr(unsafe.Pointer(p)), 0) != 0 { // IPersistFile::Load(path, STGM_READ)
		return ""
	}
	var buf [windows.MAX_PATH]uint16
	if comCall(psl, 3, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0, 0) != 0 { // GetPath
		return ""
	}
	return windows.UTF16ToString(buf[:])
}

// createLnk 은 target 을 가리키는 .lnk 파일을 dest 에 만든다.
func createLnk(target, dest string) error {
	psl, ppf, ok := newShellLink()
	if !ok {
		return fmt.Errorf("IShellLink 생성 실패")
	}
	defer comCall(psl, 2)
	defer comCall(ppf, 2)

	t, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	if hr := comCall(psl, 20, uintptr(unsafe.Pointer(t))); hr != 0 { // SetPath
		return fmt.Errorf("SetPath 실패 (0x%X)", hr)
	}
	wd, err := windows.UTF16PtrFromString(filepath.Dir(target))
	if err == nil {
		comCall(psl, 9, uintptr(unsafe.Pointer(wd))) // SetWorkingDirectory
	}
	d, err := windows.UTF16PtrFromString(dest)
	if err != nil {
		return err
	}
	if hr := comCall(ppf, 6, uintptr(unsafe.Pointer(d)), 1); hr != 0 { // IPersistFile::Save(dest, TRUE)
		return fmt.Errorf("저장 실패 (0x%X)", hr)
	}
	return nil
}

// ---- 창/공용 ----

func launch(path string) {
	op, _ := windows.UTF16PtrFromString("open")
	p, _ := windows.UTF16PtrFromString(path)
	procShellExecuteW.Call(0, uintptr(unsafe.Pointer(op)), uintptr(unsafe.Pointer(p)), 0, 0, swShowNormal)
}

func createHiddenWindow(a *app) windows.Handle {
	hInst, _, _ := procGetModuleHandleW.Call(0)
	className := utf16Ptr("gotoolLauncherWnd")

	wndProc := windows.NewCallback(func(hwnd, msg, wparam, lparam uintptr) uintptr {
		if msg == wmMenuRButtonUp {
			// wparam=항목 위치, lparam=HMENU. 삭제 가능한 항목이면 메뉴를 닫고 삭제 흐름으로.
			idr, _, _ := procGetMenuItemID.Call(lparam, wparam)
			if path, ok := a.files[uint32(idr)]; ok {
				a.rbPath = path
				procEndMenu.Call()
			}
			return 0
		}
		r, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
		return r
	})

	wc := wndClassEx{
		cbSize:        uint32(unsafe.Sizeof(wndClassEx{})),
		lpfnWndProc:   wndProc,
		hInstance:     windows.Handle(hInst),
		lpszClassName: className,
	}
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	hwnd, _, _ := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		0,
		wsPopup,
		0, 0, 0, 0,
		0, 0, hInst, 0,
	)
	return windows.Handle(hwnd)
}

func getSystemMetrics(index int32) int32 {
	r, _, _ := procGetSystemMetrics.Call(uintptr(index))
	if r == 0 {
		return 16
	}
	return int32(r)
}

func utf16Ptr(s string) *uint16 {
	p, err := windows.UTF16PtrFromString(s)
	if err != nil {
		p, _ = windows.UTF16PtrFromString("?")
	}
	return p
}

func alert(title, text string, flags uint32) {
	procMessageBoxW.Call(0,
		uintptr(unsafe.Pointer(utf16Ptr(text))),
		uintptr(unsafe.Pointer(utf16Ptr(title))),
		uintptr(flags|mbOK))
}
