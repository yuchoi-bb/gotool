//go:build windows

// gotool — 특정 폴더에 있는 즐겨찾기(바로가기)를 아이콘과 함께 팝업 메뉴로 보여주는 런처.
// 실행하면 마우스 커서 위치에 메뉴가 뜨고, 항목을 클릭하면 해당 바로가기를 실행한 뒤 종료한다.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Win32 창/메뉴는 만든 스레드에서만 다룰 수 있으므로 main 고루틴을 OS 스레드에 고정한다.
func init() {
	runtime.LockOSThread()
}

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procRegisterClassExW    = user32.NewProc("RegisterClassExW")
	procCreateWindowExW     = user32.NewProc("CreateWindowExW")
	procDefWindowProcW      = user32.NewProc("DefWindowProcW")
	procDestroyWindow       = user32.NewProc("DestroyWindow")
	procCreatePopupMenu     = user32.NewProc("CreatePopupMenu")
	procDestroyMenu         = user32.NewProc("DestroyMenu")
	procInsertMenuItemW     = user32.NewProc("InsertMenuItemW")
	procGetMenuItemCount    = user32.NewProc("GetMenuItemCount")
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

	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
)

const (
	wsPopup = 0x80000000

	miimState   = 0x0001
	miimID      = 0x0002
	miimSubMenu = 0x0004
	miimString  = 0x0040
	miimBitmap  = 0x0080

	tpmReturnCmd  = 0x0100
	tpmLeftButton = 0x0000

	shgfiIcon        = 0x000000100
	shgfiDisplayName = 0x000000200
	shgfiSmallIcon   = 0x000000001

	smCxSmIcon = 49
	smCySmIcon = 50

	diNormal     = 0x0003
	biRGB        = 0
	dibRGBColors = 0

	swShowNormal = 1

	mbIconError       = 0x00000010
	mbIconInformation = 0x00000040

	maxDepth = 4
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

type app struct {
	nextID  uint32
	cmds    map[uint32]string
	bitmaps []windows.Handle
	iconCx  int32
	iconCy  int32
}

func main() {
	// windowsgui 빌드는 콘솔이 없어 패닉이 보이지 않으므로 메시지박스로 알린다.
	defer func() {
		if r := recover(); r != nil {
			alert("gotool", fmt.Sprintf("오류가 발생했습니다:\n%v", r), mbIconError)
		}
	}()

	procSetProcessDPIAware.Call()

	dir, err := resolveDir()
	if err != nil {
		alert("gotool", err.Error(), mbIconError)
		return
	}

	a := &app{
		nextID: 1,
		cmds:   map[uint32]string{},
		iconCx: getSystemMetrics(smCxSmIcon),
		iconCy: getSystemMetrics(smCySmIcon),
	}

	menu := a.buildMenu(dir, 0)
	defer func() {
		procDestroyMenu.Call(uintptr(menu))
		for _, b := range a.bitmaps {
			procDeleteObject.Call(uintptr(b))
		}
	}()

	if len(a.cmds) == 0 {
		alert("gotool", "폴더가 비어 있습니다:\n"+dir+"\n\n이 폴더에 바로가기(.lnk, .url)나 파일을 넣어 주세요.", mbIconInformation)
		return
	}

	hwnd := createHiddenWindow()
	if hwnd == 0 {
		alert("gotool", "창을 생성하지 못했습니다.", mbIconError)
		return
	}
	defer procDestroyWindow.Call(uintptr(hwnd))

	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	procSetForegroundWindow.Call(uintptr(hwnd))

	cmd, _, callErr := procTrackPopupMenuEx.Call(
		uintptr(menu),
		tpmReturnCmd|tpmLeftButton,
		uintptr(pt.x), uintptr(pt.y),
		uintptr(hwnd), 0,
	)
	if cmd != 0 {
		if path, ok := a.cmds[uint32(cmd)]; ok {
			launch(path)
		}
		return
	}
	// cmd==0 은 대부분 사용자가 메뉴를 취소한 경우지만,
	// 실제 실패(GetLastError != 0)라면 원인을 표시한다.
	if errno, ok := callErr.(syscall.Errno); ok && errno != 0 {
		alert("gotool", fmt.Sprintf("메뉴를 표시하지 못했습니다.\n(오류 %d: %v)", uint32(errno), errno), mbIconError)
	}
}

// resolveDir 는 보여줄 폴더를 정한다.
// 1순위: 첫 번째 명령행 인자, 2순위: exe 옆의 shortcuts/favorites/즐겨찾기 폴더,
// 3순위: 사용자 즐겨찾기 폴더(%USERPROFILE%\Favorites).
func resolveDir() (string, error) {
	if len(os.Args) > 1 {
		dir := os.Args[1]
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			return dir, nil
		}
		return "", errFolder(dir)
	}

	if exe, err := os.Executable(); err == nil {
		base := filepath.Dir(exe)
		for _, name := range []string{"shortcuts", "favorites", "즐겨찾기"} {
			dir := filepath.Join(base, name)
			if st, err := os.Stat(dir); err == nil && st.IsDir() {
				return dir, nil
			}
		}
	}

	if dir, err := windows.KnownFolderPath(windows.FOLDERID_Favorites, 0); err == nil {
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			return dir, nil
		}
	}
	return "", errFolder("")
}

type errFolder string

func (e errFolder) Error() string {
	if e == "" {
		return "보여줄 폴더를 찾지 못했습니다.\n\n사용법: gotool.exe <폴더경로>\n또는 exe 옆에 shortcuts / favorites / 즐겨찾기 폴더를 만들어 주세요."
	}
	return "폴더를 찾을 수 없습니다:\n" + string(e)
}

func (a *app) buildMenu(dir string, depth int) windows.Handle {
	hMenu, _, _ := procCreatePopupMenu.Call()
	menu := windows.Handle(hMenu)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return menu
	}

	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name())
	})

	var pos uint32
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || strings.EqualFold(name, "desktop.ini") {
			continue
		}
		full := filepath.Join(dir, name)

		mii := menuItemInfo{
			cbSize: uint32(unsafe.Sizeof(menuItemInfo{})),
			fMask:  miimString,
		}

		if e.IsDir() && depth+1 < maxDepth {
			sub := a.buildMenu(full, depth+1)
			mii.fMask |= miimSubMenu
			mii.hSubMenu = sub
		} else if e.IsDir() {
			continue
		} else {
			id := a.nextID
			a.nextID++
			a.cmds[id] = full
			mii.fMask |= miimID
			mii.wID = id
		}

		label, bmp := a.shellInfo(full)
		if label == "" {
			label = displayName(name)
		}
		if bmp != 0 {
			mii.fMask |= miimBitmap
			mii.hbmpItem = bmp
		}

		text, err := windows.UTF16PtrFromString(strings.ReplaceAll(label, "&", "&&"))
		if err != nil {
			continue
		}
		mii.dwTypeData = text

		procInsertMenuItemW.Call(uintptr(menu), uintptr(pos), 1, uintptr(unsafe.Pointer(&mii)))
		pos++
	}
	return menu
}

// shellInfo 는 셸이 알려주는 표시 이름과 메뉴용 아이콘 비트맵을 돌려준다.
func (a *app) shellInfo(path string) (string, windows.Handle) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", 0
	}
	var sfi shFileInfo
	r, _, _ := procSHGetFileInfoW.Call(
		uintptr(unsafe.Pointer(p)), 0,
		uintptr(unsafe.Pointer(&sfi)), unsafe.Sizeof(sfi),
		shgfiIcon|shgfiSmallIcon|shgfiDisplayName,
	)
	if r == 0 {
		return "", 0
	}
	name := windows.UTF16ToString(sfi.szDisplayName[:])

	var bmp windows.Handle
	if sfi.hIcon != 0 {
		bmp = a.bitmapFromIcon(sfi.hIcon)
		procDestroyIcon.Call(uintptr(sfi.hIcon))
		if bmp != 0 {
			a.bitmaps = append(a.bitmaps, bmp)
		}
	}
	return name, bmp
}

// bitmapFromIcon 은 HICON을 알파 채널이 살아있는 32bpp HBITMAP으로 바꾼다(메뉴 아이콘용).
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
		biSize:        uint32(unsafe.Sizeof(bitmapInfoHeader{})),
		biWidth:       a.iconCx,
		biHeight:      -a.iconCy, // top-down
		biPlanes:      1,
		biBitCount:    32,
		biCompression: biRGB,
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

func displayName(fileName string) string {
	ext := strings.ToLower(filepath.Ext(fileName))
	if ext == ".lnk" || ext == ".url" {
		return strings.TrimSuffix(fileName, filepath.Ext(fileName))
	}
	return fileName
}

func launch(path string) {
	op, _ := windows.UTF16PtrFromString("open")
	p, _ := windows.UTF16PtrFromString(path)
	procShellExecuteW.Call(0, uintptr(unsafe.Pointer(op)), uintptr(unsafe.Pointer(p)), 0, 0, swShowNormal)
}

func createHiddenWindow() windows.Handle {
	hInst, _, _ := procGetModuleHandleW.Call(0)
	className, _ := windows.UTF16PtrFromString("gotoolLauncherWnd")

	wndProc := windows.NewCallback(func(hwnd, msg, wparam, lparam uintptr) uintptr {
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

func alert(title, text string, flags uint32) {
	t, _ := windows.UTF16PtrFromString(title)
	m, _ := windows.UTF16PtrFromString(text)
	procMessageBoxW.Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), uintptr(flags))
}
