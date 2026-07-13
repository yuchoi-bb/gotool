//go:build windows

// gotool — 즐겨찾기(바로가기) 런처.
// 실행하면 마우스 커서 위치에 우클릭 메뉴 모양의 다열 패널이 뜬다.
//   - 왼쪽 클릭: 실행(창 닫힘)
//   - 드래그 & 드롭: 다른 열로 이동하거나 같은 열 안에서 순서 변경(창은 제자리 유지)
//   - 오른쪽 클릭: 삭제(확인창)
//   - ESC 또는 바깥 클릭: 닫기
//
// 데이터는 모두 exe 옆 shortcuts 폴더에 저장된다:
//   - 바로가기 파일(.lnk/.url)과 카테고리 하위 폴더
//   - config.json  열 구성(이름/폴더/자동분류)
//   - .order       표시 순서
//   - .seen        신규(!) 표시 기록
//
// 백업(zip 내보내기)/복원 버튼으로 포맷 후에도 그대로 복구할 수 있다.
//
// 오류는 exe 옆 gotool.log 에 "시각 <TAB> 버전 <TAB> 코드 <TAB> 내용" 형식으로 기록된다.
//
// 사용법:
//
//	gotool.exe              exe 옆 shortcuts 폴더의 내용을 패널로 표시
//	gotool.exe <폴더>       지정한 폴더의 내용을 표시
//	gotool.exe add <경로>   파일/폴더를 shortcuts 폴더에 추가 (.lnk/.url은 복사, 그 외는 바로가기 생성)
//	gotool.exe install      탐색기 우클릭 메뉴에 "gotool에 추가" 등록
//	gotool.exe uninstall    우클릭 메뉴 등록 해제
package main

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	neturl "net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// 릴리스 빌드 시 ldflags 로 주입된다: -X main.version=v0.5.0
var version = "v0.0.0"

const (
	repoOwner = "yuchoi-bb"
	repoName  = "gotool"

	newBadgeAge = 10 * time.Minute // 추가된 지 이 시간 안이면 이름 오른쪽에 "!" 표시
)

// 오류 코드(gotool.log 와 오류창에 함께 표시)
const (
	errFolder   = "E01" // 폴더 생성/접근 실패
	errWindow   = "E02" // 창 생성 실패
	errMove     = "E03" // 항목 이동 실패
	errDelete   = "E04" // 항목 삭제 실패
	errAdd      = "E05" // 항목 추가 실패
	errRegistry = "E06" // 우클릭 메뉴 등록/해제 실패
	errUpdate   = "E07" // 업데이트 확인 실패(알림 없이 기록만)
	errBackup   = "E08" // 백업/복원 실패
	errConfig   = "E09" // 설정 파일(config.json) 오류
	errPanic    = "E99" // 예기치 못한 오류(패닉)
)

// Win32 창은 만든 스레드에서만 다룰 수 있으므로 main 고루틴을 OS 스레드에 고정한다.
func init() {
	runtime.LockOSThread()
}

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	ole32    = windows.NewLazySystemDLL("ole32.dll")
	kernel   = windows.NewLazySystemDLL("kernel32.dll")
	comdlg32 = windows.NewLazySystemDLL("comdlg32.dll")

	procRegisterClassExW    = user32.NewProc("RegisterClassExW")
	procCreateWindowExW     = user32.NewProc("CreateWindowExW")
	procDefWindowProcW      = user32.NewProc("DefWindowProcW")
	procDestroyWindow       = user32.NewProc("DestroyWindow")
	procShowWindow          = user32.NewProc("ShowWindow")
	procSetWindowPos        = user32.NewProc("SetWindowPos")
	procGetClientRect       = user32.NewProc("GetClientRect")
	procInvalidateRect      = user32.NewProc("InvalidateRect")
	procBeginPaint          = user32.NewProc("BeginPaint")
	procEndPaint            = user32.NewProc("EndPaint")
	procFillRect            = user32.NewProc("FillRect")
	procFrameRect           = user32.NewProc("FrameRect")
	procDrawTextW           = user32.NewProc("DrawTextW")
	procGetSysColor         = user32.NewProc("GetSysColor")
	procGetSysColorBrush    = user32.NewProc("GetSysColorBrush")
	procSetCapture          = user32.NewProc("SetCapture")
	procReleaseCapture      = user32.NewProc("ReleaseCapture")
	procSetCursor           = user32.NewProc("SetCursor")
	procLoadCursorW         = user32.NewProc("LoadCursorW")
	procGetMessageW         = user32.NewProc("GetMessageW")
	procTranslateMessage    = user32.NewProc("TranslateMessage")
	procDispatchMessageW    = user32.NewProc("DispatchMessageW")
	procPostQuitMessage     = user32.NewProc("PostQuitMessage")
	procPostMessageW        = user32.NewProc("PostMessageW")
	procGetCursorPos        = user32.NewProc("GetCursorPos")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procMessageBoxW         = user32.NewProc("MessageBoxW")
	procGetSystemMetrics    = user32.NewProc("GetSystemMetrics")
	procSetProcessDPIAware  = user32.NewProc("SetProcessDPIAware")
	procDrawIconEx          = user32.NewProc("DrawIconEx")
	procDestroyIcon         = user32.NewProc("DestroyIcon")
	procGetDC               = user32.NewProc("GetDC")
	procReleaseDC           = user32.NewProc("ReleaseDC")

	procCreateCompatibleDC     = gdi32.NewProc("CreateCompatibleDC")
	procCreateCompatibleBitmap = gdi32.NewProc("CreateCompatibleBitmap")
	procDeleteDC               = gdi32.NewProc("DeleteDC")
	procSelectObject           = gdi32.NewProc("SelectObject")
	procDeleteObject           = gdi32.NewProc("DeleteObject")
	procBitBlt                 = gdi32.NewProc("BitBlt")
	procSetBkMode              = gdi32.NewProc("SetBkMode")
	procSetTextColor           = gdi32.NewProc("SetTextColor")
	procCreateFontW            = gdi32.NewProc("CreateFontW")
	procGetTextExtentPoint32W  = gdi32.NewProc("GetTextExtentPoint32W")
	procGetDeviceCaps          = gdi32.NewProc("GetDeviceCaps")

	procSHGetFileInfoW = shell32.NewProc("SHGetFileInfoW")
	procShellExecuteW  = shell32.NewProc("ShellExecuteW")

	procCoInitializeEx   = ole32.NewProc("CoInitializeEx")
	procCoUninitialize   = ole32.NewProc("CoUninitialize")
	procCoCreateInstance = ole32.NewProc("CoCreateInstance")

	procGetModuleHandleW = kernel.NewProc("GetModuleHandleW")

	procGetSaveFileNameW = comdlg32.NewProc("GetSaveFileNameW")
	procGetOpenFileNameW = comdlg32.NewProc("GetOpenFileNameW")
)

const (
	wsPopup  = 0x80000000
	wsBorder = 0x00800000

	wsExTopmost    = 0x00000008
	wsExToolWindow = 0x00000080

	swShow = 5

	swpNoMove   = 0x0002
	swpNoZorder = 0x0004

	wmDestroy     = 0x0002
	wmActivate    = 0x0006
	wmSetCursor   = 0x0020
	wmEraseBkgnd  = 0x0014
	wmPaint       = 0x000F
	wmKeyDown     = 0x0100
	wmMouseMove   = 0x0200
	wmLButtonDown = 0x0201
	wmLButtonUp   = 0x0202
	wmRButtonUp   = 0x0205
	wmAppUpdate   = 0x8000 + 1 // 업데이트 확인 완료(고루틴 → UI 스레드)

	waInactive = 0
	vkEscape   = 0x1B

	idcArrow   = 32512
	idcSizeAll = 32646

	dtFlags = 0x0020 | 0x0004 | 0x8000 | 0x0800 // DT_SINGLELINE|DT_VCENTER|DT_END_ELLIPSIS|DT_NOPREFIX

	colorMenu          = 4
	colorMenuText      = 7
	colorHighlight     = 13
	colorHighlightText = 14
	color3DShadow      = 16
	colorGrayText      = 17

	srcCopy       = 0x00CC0020
	bkTransparent = 1
	logPixelsY    = 90

	shgfiIcon      = 0x00000100
	shgfiSmallIcon = 0x00000001

	smCxSmIcon        = 49
	smCySmIcon        = 50
	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCxVirtualScreen = 78
	smCyVirtualScreen = 79

	diNormal = 0x0003

	swShowNormal = 1

	mbOK              = 0x00000000
	mbYesNo           = 0x00000004
	mbIconError       = 0x00000010
	mbIconWarning     = 0x00000030
	mbIconInformation = 0x00000040
	idYes             = 6

	clsctxInprocServer = 1
	coinitApartment    = 2

	ofnOverwritePrompt = 0x00000002
	ofnFileMustExist   = 0x00001000
)

// ---- 설정(config.json): 열 구성 ----

type column struct {
	Name   string `json:"name"`           // 열 제목(패널에 표시)
	Folder string `json:"folder"`         // shortcuts 안의 하위 폴더 이름
	Auto   string `json:"auto,omitempty"` // 자동 분류 대상: "web" | "dev" | "etc" | ""(수동 전용)
}

type appConfig struct {
	Columns     []column `json:"columns"`
	DevKeywords []string `json:"devKeywords,omitempty"` // "dev" 자동 분류 키워드(비우면 기본값)
}

func defaultConfig() appConfig {
	return appConfig{
		Columns: []column{
			{Name: "🌐 웹", Folder: "웹", Auto: "web"},
			{Name: "💻 개발", Folder: "개발", Auto: "dev"},
			{Name: "📦 비개발", Folder: "비개발", Auto: "etc"},
			{Name: "🐿 다람쥐", Folder: "다람쥐"},
		},
	}
}

var defaultDevKeywords = []string{
	"code", "studio", "git", "docker", "python", "pycharm", "intellij", "idea",
	"webstorm", "goland", "clion", "rider", "devenv", "node", "npm", "sql",
	"vim", "sublime", "eclipse", "unity", "unreal", "android", "sdk",
	"terminal", "cmd", "powershell", "putty", "ssh", "postman", "notepad++",
	"dbeaver", "cursor", "wsl", "개발",
}

func configPath(dataDir string) string {
	return filepath.Join(dataDir, "config.json")
}

// loadConfig 는 config.json 을 읽는다. 없으면 기본값으로 만들어 저장한다.
func loadConfig(dataDir string) appConfig {
	p := configPath(dataDir)
	data, err := os.ReadFile(p)
	if err != nil {
		cfg := defaultConfig()
		saveConfig(dataDir, cfg)
		return cfg
	}
	var cfg appConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		alertErr(errConfig, "설정 파일(config.json)을 읽을 수 없어 기본 구성으로 실행합니다", err)
		return defaultConfig()
	}
	// 잘못된 항목 정리
	var cols []column
	for _, c := range cfg.Columns {
		c.Name = strings.TrimSpace(c.Name)
		c.Folder = strings.TrimSpace(c.Folder)
		if c.Folder == "" || strings.ContainsAny(c.Folder, `\/:*?"<>|`) {
			continue
		}
		if c.Name == "" {
			c.Name = c.Folder
		}
		cols = append(cols, c)
	}
	if len(cols) == 0 {
		alertErr(errConfig, "설정 파일에 유효한 열이 없어 기본 구성으로 실행합니다", nil)
		return defaultConfig()
	}
	cfg.Columns = cols
	return cfg
}

func saveConfig(dataDir string, cfg appConfig) {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(configPath(dataDir), data, 0o644); err != nil {
		logErr(errConfig, "config.json 저장 실패: "+err.Error())
	}
}

// ---- 구조체 ----

type point struct{ x, y int32 }
type gdiSize struct{ cx, cy int32 }
type rect struct{ left, top, right, bottom int32 }

func (r rect) contains(x, y int32) bool {
	return x >= r.left && x < r.right && y >= r.top && y < r.bottom
}

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

type msgStruct struct {
	hwnd    windows.Handle
	message uint32
	wparam  uintptr
	lparam  uintptr
	time    uint32
	pt      point
}

type paintStruct struct {
	hdc         windows.Handle
	fErase      int32
	rcPaint     rect
	fRestore    int32
	fIncUpdate  int32
	rgbReserved [32]byte
}

type shFileInfo struct {
	hIcon         windows.Handle
	iIcon         int32
	dwAttributes  uint32
	szDisplayName [260]uint16
	szTypeName    [80]uint16
}

type openFileNameW struct {
	lStructSize       uint32
	_                 uint32
	hwndOwner         windows.Handle
	hInstance         windows.Handle
	lpstrFilter       *uint16
	lpstrCustomFilter *uint16
	nMaxCustFilter    uint32
	nFilterIndex      uint32
	lpstrFile         *uint16
	nMaxFile          uint32
	_                 uint32
	lpstrFileTitle    *uint16
	nMaxFileTitle     uint32
	_                 uint32
	lpstrInitialDir   *uint16
	lpstrTitle        *uint16
	flags             uint32
	nFileOffset       uint16
	nFileExtension    uint16
	lpstrDefExt       *uint16
	lCustData         uintptr
	lpfnHook          uintptr
	lpTemplateName    *uint16
	pvReserved        uintptr
	dwReserved        uint32
	flagsEx           uint32
}

type uiItem struct {
	label string
	path  string // ShellExecute 대상(바로가기 파일 자체)
	rel   string // dataDir 기준 상대 경로(순서 저장용)
	icon  windows.Handle
	rc    rect
}

type updateInfo struct {
	tag string
	url string
}

// 히트 대상 종류
const (
	hitNone = iota
	hitItem
	hitUpdate
	hitSettings
	hitOpen
	hitBackup
	hitRestore
	hitInstall
)

type hit struct {
	kind int
	cat  int
	idx  int
}

type button struct {
	kind  int
	label string
	rc    rect
}

type app struct {
	dataDir string
	cols    []column
	devKeys []string
	hwnd    windows.Handle
	font    windows.Handle
	iconCx  int32
	iconCy  int32
	dpi     int32

	items    [][]uiItem
	colBand  []rect
	headerRc []rect
	buttons  []button
	updRc    rect
	winW     int32
	winH     int32

	hover    hit
	pressed  hit
	pressPt  point
	dragging bool
	dropCat  int
	dropIdx  int

	modal bool // 대화상자 표시 중(비활성화로 창이 닫히지 않게)

	update   *updateInfo
	updateCh chan *updateInfo
}

func main() {
	// windowsgui 빌드는 콘솔이 없어 패닉이 보이지 않으므로 기록하고 메시지박스로 알린다.
	defer func() {
		if r := recover(); r != nil {
			alertErr(errPanic, fmt.Sprintf("예기치 못한 오류가 발생했습니다:\n%v", r), nil)
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
				alertErr(errRegistry, "우클릭 메뉴 등록 실패", err)
			} else {
				alert("gotool", "탐색기 우클릭 메뉴에 \"gotool에 추가\"를 등록했습니다.\n\nWindows 11에서는 우클릭 후 \"더 많은 옵션 표시\" 안에 나타납니다.", mbIconInformation)
			}
			return
		case "uninstall":
			if err := uninstallContextMenu(); err != nil {
				alertErr(errRegistry, "우클릭 메뉴 해제 실패", err)
			} else {
				alert("gotool", "우클릭 메뉴 등록을 해제했습니다.", mbIconInformation)
			}
			return
		}
	}

	dir, err := resolveDir(args)
	if err != nil {
		alertErr(errFolder, err.Error(), nil)
		return
	}
	runPanel(dir)
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

// ---- 패널 창 ----

func runPanel(dir string) {
	a := &app{
		dataDir:  dir,
		iconCx:   getSystemMetrics(smCxSmIcon, 16),
		iconCy:   getSystemMetrics(smCySmIcon, 16),
		updateCh: make(chan *updateInfo, 1),
		hover:    hit{kind: hitNone},
		pressed:  hit{kind: hitNone},
		dropCat:  -1,
		dropIdx:  -1,
	}

	hdcScreen, _, _ := procGetDC.Call(0)
	dpi, _, _ := procGetDeviceCaps.Call(hdcScreen, logPixelsY)
	procReleaseDC.Call(0, hdcScreen)
	a.dpi = int32(dpi)
	if a.dpi <= 0 {
		a.dpi = 96
	}
	a.font = createUIFont(a.dpi)
	defer procDeleteObject.Call(uintptr(a.font))

	if !a.createWindow() {
		alertErr(errWindow, "창을 생성하지 못했습니다", nil)
		return
	}

	a.reload()

	// 커서 위치에 표시(화면 밖으로 나가지 않게 보정)
	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	vx := getSystemMetrics(smXVirtualScreen, 0)
	vy := getSystemMetrics(smYVirtualScreen, 0)
	vw := getSystemMetrics(smCxVirtualScreen, 1920)
	vh := getSystemMetrics(smCyVirtualScreen, 1080)
	x := pt.x
	y := pt.y
	if x+a.winW > vx+vw {
		x = vx + vw - a.winW
	}
	if y+a.winH > vy+vh {
		y = vy + vh - a.winH
	}
	if x < vx {
		x = vx
	}
	if y < vy {
		y = vy
	}
	procSetWindowPos.Call(uintptr(a.hwnd), 0, uintptr(x), uintptr(y), uintptr(a.winW), uintptr(a.winH), swpNoZorder)
	procShowWindow.Call(uintptr(a.hwnd), swShow)
	procSetForegroundWindow.Call(uintptr(a.hwnd))

	// 업데이트 확인은 백그라운드로. 끝나면 UI 스레드에 알림.
	go func() {
		checkUpdate(a.updateCh)
		procPostMessageW.Call(uintptr(a.hwnd), wmAppUpdate, 0, 0)
	}()

	var m msgStruct
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if r == 0 || int32(r) == -1 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
	a.freeIcons()
}

func (a *app) createWindow() bool {
	hInst, _, _ := procGetModuleHandleW.Call(0)
	className := utf16Ptr("gotoolPanelWnd")
	arrow, _, _ := procLoadCursorW.Call(0, idcArrow)

	wndProc := windows.NewCallback(a.wndProc)
	wc := wndClassEx{
		cbSize:        uint32(unsafe.Sizeof(wndClassEx{})),
		lpfnWndProc:   wndProc,
		hInstance:     windows.Handle(hInst),
		hCursor:       windows.Handle(arrow),
		lpszClassName: className,
	}
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	hwnd, _, _ := procCreateWindowExW.Call(
		wsExTopmost|wsExToolWindow,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(utf16Ptr("gotool"))),
		wsPopup|wsBorder,
		0, 0, 100, 100,
		0, 0, hInst, 0,
	)
	a.hwnd = windows.Handle(hwnd)
	return hwnd != 0
}

func (a *app) wndProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	switch msg {
	case wmPaint:
		var ps paintStruct
		hdc, _, _ := procBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		a.paint(hdc)
		procEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&ps)))
		return 0
	case wmEraseBkgnd:
		return 1
	case wmMouseMove:
		x, y := mouseXY(lparam)
		a.onMouseMove(x, y)
		return 0
	case wmLButtonDown:
		x, y := mouseXY(lparam)
		a.onLButtonDown(x, y)
		return 0
	case wmLButtonUp:
		x, y := mouseXY(lparam)
		a.onLButtonUp(x, y)
		return 0
	case wmRButtonUp:
		x, y := mouseXY(lparam)
		a.onRButtonUp(x, y)
		return 0
	case wmSetCursor:
		if a.dragging {
			c, _, _ := procLoadCursorW.Call(0, idcSizeAll)
			procSetCursor.Call(c)
			return 1
		}
	case wmKeyDown:
		if wparam == vkEscape {
			procDestroyWindow.Call(hwnd)
			return 0
		}
	case wmActivate:
		if wparam&0xFFFF == waInactive && !a.modal && !a.dragging {
			procDestroyWindow.Call(hwnd)
			return 0
		}
	case wmAppUpdate:
		select {
		case u := <-a.updateCh:
			if u != nil {
				a.update = u
				a.refresh()
			}
		default:
		}
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
	return r
}

func mouseXY(lparam uintptr) (int32, int32) {
	return int32(int16(lparam & 0xFFFF)), int32(int16((lparam >> 16) & 0xFFFF))
}

func (a *app) scale(n int32) int32 { return n * a.dpi / 96 }

// reload 는 설정/폴더를 다시 읽고 아이콘/레이아웃을 계산한다.
func (a *app) reload() {
	a.freeIcons()

	cfg := loadConfig(a.dataDir)
	a.cols = cfg.Columns
	a.devKeys = cfg.DevKeywords
	if len(a.devKeys) == 0 {
		a.devKeys = defaultDevKeywords
	}

	cats := a.scan()
	a.items = make([][]uiItem, len(a.cols))
	for ci := range a.cols {
		for _, it := range cats[ci] {
			a.items[ci] = append(a.items[ci], uiItem{
				label: it.label,
				path:  it.path,
				rel:   it.rel,
				icon:  iconHandle(it.iconSrc),
			})
		}
	}
	a.saveOrderFromItems()
	a.layout()
}

func (a *app) freeIcons() {
	for ci := range a.items {
		for _, it := range a.items[ci] {
			if it.icon != 0 {
				procDestroyIcon.Call(uintptr(it.icon))
			}
		}
	}
	a.items = nil
}

// layout 은 열 너비/항목 좌표/버튼 좌표와 창 크기를 계산한다.
func (a *app) layout() {
	hdc, _, _ := procGetDC.Call(uintptr(a.hwnd))
	oldFont, _, _ := procSelectObject.Call(hdc, uintptr(a.font))

	pad := a.scale(8)
	rowH := a.iconCy + a.scale(10)
	sepW := a.scale(11)
	iconPad := a.scale(6)
	minCol := a.scale(130)
	maxCol := a.scale(330)

	textW := func(s string) int32 {
		u, err := windows.UTF16FromString(s)
		if err != nil || len(u) <= 1 {
			return 0
		}
		var sz gdiSize
		procGetTextExtentPoint32W.Call(hdc, uintptr(unsafe.Pointer(&u[0])), uintptr(len(u)-1), uintptr(unsafe.Pointer(&sz)))
		return sz.cx
	}

	n := len(a.cols)
	a.headerRc = make([]rect, n)
	a.colBand = make([]rect, n)

	colW := make([]int32, n)
	maxRows := 1
	for ci := 0; ci < n; ci++ {
		w := textW(a.cols[ci].Name)
		for _, it := range a.items[ci] {
			if tw := textW(it.label); tw > w {
				w = tw
			}
		}
		w += a.iconCx + iconPad + pad*2
		if w < minCol {
			w = minCol
		}
		if w > maxCol {
			w = maxCol
		}
		colW[ci] = w
		if r := len(a.items[ci]); r > maxRows {
			maxRows = r
		}
	}

	topH := int32(0)
	if a.update != nil {
		topH = rowH + a.scale(6)
	}

	colTop := pad + topH
	itemsBottom := colTop + rowH + int32(maxRows)*rowH // 헤더 + 항목들

	x := pad
	for ci := 0; ci < n; ci++ {
		a.headerRc[ci] = rect{x, colTop, x + colW[ci], colTop + rowH}
		y := colTop + rowH
		for i := range a.items[ci] {
			a.items[ci][i].rc = rect{x, y, x + colW[ci], y + rowH}
			y += rowH
		}
		a.colBand[ci] = rect{x, colTop, x + colW[ci], itemsBottom}
		x += colW[ci] + sepW
	}
	winW := x - sepW + pad

	// 하단 관리 버튼
	labels := []struct {
		kind  int
		label string
	}{
		{hitSettings, "⚙ 설정 (열 이름·구성 편집)"},
		{hitOpen, "📁 바로가기 폴더 열기"},
		{hitBackup, "💾 백업 저장 (zip)"},
		{hitRestore, "📂 백업 불러오기"},
	}
	if !contextMenuInstalled() {
		labels = append(labels, struct {
			kind  int
			label string
		}{hitInstall, "🖱 우클릭 \"gotool에 추가\" 메뉴 등록"})
	}
	a.buttons = a.buttons[:0]
	y := itemsBottom + a.scale(6)
	for _, l := range labels {
		a.buttons = append(a.buttons, button{kind: l.kind, label: l.label, rc: rect{pad, y, winW - pad, y + rowH}})
		y += rowH
	}

	if a.update != nil {
		a.updRc = rect{pad, pad, winW - pad, pad + rowH}
	} else {
		a.updRc = rect{}
	}

	a.winW = winW
	a.winH = y + pad

	procSelectObject.Call(hdc, oldFont)
	procReleaseDC.Call(uintptr(a.hwnd), hdc)
}

// refresh 는 재스캔 후 창 크기만 갱신하고(위치 유지) 다시 그린다.
func (a *app) refresh() {
	a.reload()
	procSetWindowPos.Call(uintptr(a.hwnd), 0, 0, 0, uintptr(a.winW), uintptr(a.winH), swpNoMove|swpNoZorder)
	procInvalidateRect.Call(uintptr(a.hwnd), 0, 1)
}

// ---- 그리기 ----

func (a *app) paint(hdc uintptr) {
	var rc rect
	procGetClientRect.Call(uintptr(a.hwnd), uintptr(unsafe.Pointer(&rc)))
	w, h := rc.right-rc.left, rc.bottom-rc.top

	memDC, _, _ := procCreateCompatibleDC.Call(hdc)
	memBmp, _, _ := procCreateCompatibleBitmap.Call(hdc, uintptr(w), uintptr(h))
	oldBmp, _, _ := procSelectObject.Call(memDC, memBmp)
	oldFont, _, _ := procSelectObject.Call(memDC, uintptr(a.font))
	procSetBkMode.Call(memDC, bkTransparent)

	brush := func(idx uintptr) uintptr { b, _, _ := procGetSysColorBrush.Call(idx); return b }
	color := func(idx uintptr) uintptr { c, _, _ := procGetSysColor.Call(idx); return c }

	procFillRect.Call(memDC, uintptr(unsafe.Pointer(&rc)), brush(colorMenu))

	drawText := func(r rect, s string, col uintptr) {
		u, err := windows.UTF16FromString(s)
		if err != nil {
			return
		}
		procSetTextColor.Call(memDC, col)
		procDrawTextW.Call(memDC, uintptr(unsafe.Pointer(&u[0])), ^uintptr(0), uintptr(unsafe.Pointer(&r)), dtFlags)
	}

	rowText := func(r rect, label string, icon windows.Handle, hovered, grayed bool) {
		if hovered {
			procFillRect.Call(memDC, uintptr(unsafe.Pointer(&r)), brush(colorHighlight))
		}
		x := r.left + a.scale(8)
		if icon != 0 {
			iy := r.top + (r.bottom-r.top-a.iconCy)/2
			procDrawIconEx.Call(memDC, uintptr(x), uintptr(iy), uintptr(icon), uintptr(a.iconCx), uintptr(a.iconCy), 0, 0, diNormal)
		}
		tr := r
		tr.left = x + a.iconCx + a.scale(6)
		col := color(colorMenuText)
		if grayed {
			col = color(colorGrayText)
		}
		if hovered {
			col = color(colorHighlightText)
		}
		drawText(tr, label, col)
	}

	// 업데이트 버튼
	if a.update != nil {
		rowText(a.updRc, "⬆ 새 버전 "+a.update.tag+" 다운로드", 0, a.hover.kind == hitUpdate, false)
		line := rect{a.updRc.left, a.updRc.bottom + a.scale(2), a.updRc.right, a.updRc.bottom + a.scale(3)}
		procFillRect.Call(memDC, uintptr(unsafe.Pointer(&line)), brush(color3DShadow))
	}

	// 열
	for ci := range a.cols {
		hr := a.headerRc[ci]
		drawText(rect{hr.left + a.scale(8), hr.top, hr.right, hr.bottom}, a.cols[ci].Name, color(colorGrayText))
		line := rect{hr.left, hr.bottom - a.scale(2), hr.right, hr.bottom - a.scale(1)}
		procFillRect.Call(memDC, uintptr(unsafe.Pointer(&line)), brush(color3DShadow))

		if len(a.items[ci]) == 0 {
			r := rect{hr.left, hr.bottom, hr.right, hr.bottom + a.iconCy + a.scale(10)}
			drawText(rect{r.left + a.scale(8), r.top, r.right, r.bottom}, "(비어 있음)", color(colorGrayText))
		}
		for i, it := range a.items[ci] {
			hovered := !a.dragging && a.hover.kind == hitItem && a.hover.cat == ci && a.hover.idx == i
			graying := a.dragging && a.pressed.kind == hitItem && a.pressed.cat == ci && a.pressed.idx == i
			rowText(it.rc, it.label, it.icon, hovered, graying)
		}
		if ci < len(a.cols)-1 {
			sx := a.colBand[ci].right + a.scale(5)
			line := rect{sx, a.colBand[ci].top, sx + a.scale(1), a.colBand[ci].bottom}
			procFillRect.Call(memDC, uintptr(unsafe.Pointer(&line)), brush(color3DShadow))
		}
	}

	// 드래그 중: 드롭 대상 열 강조 + 삽입 위치 표시
	if a.dragging && a.dropCat >= 0 {
		band := a.colBand[a.dropCat]
		procFrameRect.Call(memDC, uintptr(unsafe.Pointer(&band)), brush(colorHighlight))
		inner := rect{band.left + 1, band.top + 1, band.right - 1, band.bottom - 1}
		procFrameRect.Call(memDC, uintptr(unsafe.Pointer(&inner)), brush(colorHighlight))

		if a.dropIdx >= 0 {
			rowH := a.iconCy + a.scale(10)
			ly := a.headerRc[a.dropCat].bottom + int32(a.dropIdx)*rowH
			mark := rect{band.left + a.scale(2), ly - a.scale(1), band.right - a.scale(2), ly + a.scale(2)}
			procFillRect.Call(memDC, uintptr(unsafe.Pointer(&mark)), brush(colorHighlight))
		}
	}

	// 하단 관리 버튼
	if len(a.buttons) > 0 {
		top := a.buttons[0].rc.top
		line := rect{a.buttons[0].rc.left, top - a.scale(3), a.buttons[0].rc.right, top - a.scale(2)}
		procFillRect.Call(memDC, uintptr(unsafe.Pointer(&line)), brush(color3DShadow))
	}
	for _, b := range a.buttons {
		rowText(b.rc, b.label, 0, a.hover.kind == b.kind, false)
	}

	procBitBlt.Call(hdc, 0, 0, uintptr(w), uintptr(h), memDC, 0, 0, srcCopy)

	procSelectObject.Call(memDC, oldFont)
	procSelectObject.Call(memDC, oldBmp)
	procDeleteObject.Call(memBmp)
	procDeleteDC.Call(memDC)
}

// ---- 마우스 처리 ----

func (a *app) hitTest(x, y int32) hit {
	if a.update != nil && a.updRc.contains(x, y) {
		return hit{kind: hitUpdate}
	}
	for _, b := range a.buttons {
		if b.rc.contains(x, y) {
			return hit{kind: b.kind}
		}
	}
	for ci := range a.items {
		for i := range a.items[ci] {
			if a.items[ci][i].rc.contains(x, y) {
				return hit{kind: hitItem, cat: ci, idx: i}
			}
		}
	}
	return hit{kind: hitNone}
}

func (a *app) onMouseMove(x, y int32) {
	if a.pressed.kind == hitItem {
		dx, dy := x-a.pressPt.x, y-a.pressPt.y
		if !a.dragging && (dx*dx+dy*dy) > a.scale(4)*a.scale(4) {
			a.dragging = true
		}
	}
	if a.dragging {
		drop, idx := -1, -1
		for ci := range a.colBand {
			if a.colBand[ci].contains(x, y) {
				drop = ci
				rowH := a.iconCy + a.scale(10)
				idx = int((y - a.headerRc[ci].bottom + rowH/2) / rowH)
				if idx < 0 {
					idx = 0
				}
				if idx > len(a.items[ci]) {
					idx = len(a.items[ci])
				}
				break
			}
		}
		if drop != a.dropCat || idx != a.dropIdx {
			a.dropCat = drop
			a.dropIdx = idx
			procInvalidateRect.Call(uintptr(a.hwnd), 0, 0)
		}
		return
	}
	h := a.hitTest(x, y)
	if h != a.hover {
		a.hover = h
		procInvalidateRect.Call(uintptr(a.hwnd), 0, 0)
	}
}

func (a *app) onLButtonDown(x, y int32) {
	a.pressed = a.hitTest(x, y)
	a.pressPt = point{x, y}
	a.dragging = false
	a.dropCat = -1
	a.dropIdx = -1
	if a.pressed.kind != hitNone {
		procSetCapture.Call(uintptr(a.hwnd))
	}
}

func (a *app) onLButtonUp(x, y int32) {
	procReleaseCapture.Call()

	if a.dragging {
		if a.pressed.kind == hitItem && a.dropCat >= 0 {
			a.dropItem(a.pressed.cat, a.pressed.idx, a.dropCat, a.dropIdx)
		}
		a.dragging = false
		a.dropCat = -1
		a.dropIdx = -1
		a.pressed = hit{kind: hitNone}
		a.refresh()
		return
	}

	prev := a.pressed
	a.pressed = hit{kind: hitNone}
	if prev.kind == hitNone || a.hitTest(x, y) != prev {
		return
	}

	switch prev.kind {
	case hitItem:
		launch(a.items[prev.cat][prev.idx].path)
		procDestroyWindow.Call(uintptr(a.hwnd))
	case hitUpdate:
		if a.update != nil {
			launch(a.update.url)
		}
		procDestroyWindow.Call(uintptr(a.hwnd))
	case hitSettings:
		launchWith("notepad.exe", configPath(a.dataDir))
	case hitOpen:
		launch(a.dataDir)
		procDestroyWindow.Call(uintptr(a.hwnd))
	case hitBackup:
		a.doBackup()
	case hitRestore:
		a.doRestore()
	case hitInstall:
		a.modal = true
		if err := installContextMenu(); err != nil {
			alertErr(errRegistry, "우클릭 메뉴 등록 실패", err)
		} else {
			alert("gotool", "탐색기 우클릭 메뉴에 \"gotool에 추가\"를 등록했습니다.\n\nWindows 11에서는 우클릭 후 \"더 많은 옵션 표시\" 안에 나타납니다.", mbIconInformation)
		}
		a.modal = false
		procSetForegroundWindow.Call(uintptr(a.hwnd))
		a.refresh()
	}
}

func (a *app) onRButtonUp(x, y int32) {
	h := a.hitTest(x, y)
	if h.kind != hitItem {
		return
	}
	a.confirmDelete(a.items[h.cat][h.idx].path)
	a.hover = hit{kind: hitNone}
	a.refresh()
}

// dropItem 은 드래그 결과를 반영한다: 같은 열이면 순서 변경, 다른 열이면 파일 이동 + 위치 지정.
func (a *app) dropItem(srcCat, srcIdx, dstCat, dstIdx int) {
	if srcCat < 0 || srcCat >= len(a.items) || srcIdx < 0 || srcIdx >= len(a.items[srcCat]) {
		return
	}
	if dstIdx < 0 {
		dstIdx = len(a.items[dstCat])
	}
	it := a.items[srcCat][srcIdx]

	if dstCat == srcCat {
		// 행간 이동(순서 변경)
		if dstIdx > srcIdx {
			dstIdx--
		}
		if dstIdx == srcIdx {
			return
		}
		col := a.items[srcCat]
		col = append(col[:srcIdx], col[srcIdx+1:]...)
		if dstIdx > len(col) {
			dstIdx = len(col)
		}
		col = append(col[:dstIdx], append([]uiItem{it}, col[dstIdx:]...)...)
		a.items[srcCat] = col
		a.saveOrderFromItems()
		return
	}

	// 열간 이동: 파일을 대상 열 폴더로 옮기고 원하는 위치에 삽입
	dir := filepath.Join(a.dataDir, a.cols[dstCat].Folder)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		a.modalAlertErr(errFolder, "폴더를 만들 수 없습니다", err)
		return
	}
	dest := uniqueDest(dir, filepath.Base(it.path))
	if err := os.Rename(it.path, dest); err != nil {
		a.modalAlertErr(errMove, "이동하지 못했습니다", err)
		return
	}
	rel, err := filepath.Rel(a.dataDir, dest)
	if err != nil {
		rel = filepath.Base(dest)
	}
	it.path = dest
	it.rel = rel

	src := a.items[srcCat]
	a.items[srcCat] = append(src[:srcIdx], src[srcIdx+1:]...)
	dst := a.items[dstCat]
	if dstIdx > len(dst) {
		dstIdx = len(dst)
	}
	a.items[dstCat] = append(dst[:dstIdx], append([]uiItem{it}, dst[dstIdx:]...)...)
	a.saveOrderFromItems()
}

// confirmDelete 는 확인 후 바로가기를 삭제한다. 안전을 위해 데이터 폴더 안의 항목만 지운다.
func (a *app) confirmDelete(path string) {
	rel, err := filepath.Rel(a.dataDir, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return
	}
	name := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	a.modal = true
	r, _, _ := procMessageBoxW.Call(uintptr(a.hwnd),
		uintptr(unsafe.Pointer(utf16Ptr(fmt.Sprintf("\"%s\" 을(를) 삭제할까요?\n\n%s", name, path)))),
		uintptr(unsafe.Pointer(utf16Ptr("gotool - 바로가기 삭제"))),
		mbYesNo|mbIconWarning)
	a.modal = false
	procSetForegroundWindow.Call(uintptr(a.hwnd))
	if r != idYes {
		return
	}
	if err := os.RemoveAll(path); err != nil {
		a.modalAlertErr(errDelete, "삭제하지 못했습니다", err)
	}
}

func (a *app) modalAlertErr(code, msg string, err error) {
	a.modal = true
	alertErr(code, msg, err)
	a.modal = false
	procSetForegroundWindow.Call(uintptr(a.hwnd))
}

// ---- 백업/복원 ----

func (a *app) doBackup() {
	a.modal = true
	dest := saveFileDialog(a.hwnd, "gotool 백업 저장", "백업 파일 (*.zip)", "*.zip",
		fmt.Sprintf("gotool-backup-%s.zip", time.Now().Format("20060102")), "zip")
	a.modal = false
	procSetForegroundWindow.Call(uintptr(a.hwnd))
	if dest == "" {
		return
	}
	if err := backupZip(a.dataDir, dest); err != nil {
		a.modalAlertErr(errBackup, "백업을 저장하지 못했습니다", err)
		return
	}
	a.modal = true
	alert("gotool", "백업을 저장했습니다:\n"+dest+"\n\n포맷/재설치 후 gotool의 \"백업 불러오기\"로 복원할 수 있습니다.", mbIconInformation)
	a.modal = false
	procSetForegroundWindow.Call(uintptr(a.hwnd))
}

func (a *app) doRestore() {
	a.modal = true
	src := openFileDialog(a.hwnd, "gotool 백업 불러오기", "백업 파일 (*.zip)", "*.zip")
	a.modal = false
	procSetForegroundWindow.Call(uintptr(a.hwnd))
	if src == "" {
		return
	}
	if err := restoreZip(src, a.dataDir); err != nil {
		a.modalAlertErr(errBackup, "백업을 불러오지 못했습니다", err)
		return
	}
	a.modal = true
	alert("gotool", "백업을 불러왔습니다. (같은 이름의 파일은 덮어씀)", mbIconInformation)
	a.modal = false
	procSetForegroundWindow.Call(uintptr(a.hwnd))
	a.refresh()
}

func backupZip(dataDir, dest string) error {
	zf, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer zf.Close()
	zw := zip.NewWriter(zf)
	defer zw.Close()

	return filepath.WalkDir(dataDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dataDir, p)
		if err != nil {
			return err
		}
		w, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	})
}

func restoreZip(src, dataDir string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()

	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rel := filepath.FromSlash(f.Name)
		if rel == "" || strings.Contains(rel, "..") || filepath.IsAbs(rel) {
			continue
		}
		dest := filepath.Join(dataDir, rel)
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return err
		}
		if err := os.WriteFile(dest, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// ---- 파일 대화상자 ----

func dialogFilter(desc, pattern string) *uint16 {
	var buf []uint16
	d, _ := windows.UTF16FromString(desc)
	p, _ := windows.UTF16FromString(pattern)
	buf = append(buf, d...) // 널 포함
	buf = append(buf, p...) // 널 포함
	buf = append(buf, 0)    // 이중 널 종료
	return &buf[0]
}

func saveFileDialog(owner windows.Handle, title, filterDesc, filterPat, defName, defExt string) string {
	var file [4096]uint16
	n, _ := windows.UTF16FromString(defName)
	copy(file[:], n)

	ofn := openFileNameW{
		lStructSize: uint32(unsafe.Sizeof(openFileNameW{})),
		hwndOwner:   owner,
		lpstrFilter: dialogFilter(filterDesc, filterPat),
		lpstrFile:   &file[0],
		nMaxFile:    uint32(len(file)),
		lpstrTitle:  utf16Ptr(title),
		lpstrDefExt: utf16Ptr(defExt),
		flags:       ofnOverwritePrompt,
	}
	r, _, _ := procGetSaveFileNameW.Call(uintptr(unsafe.Pointer(&ofn)))
	if r == 0 {
		return ""
	}
	return windows.UTF16ToString(file[:])
}

func openFileDialog(owner windows.Handle, title, filterDesc, filterPat string) string {
	var file [4096]uint16
	ofn := openFileNameW{
		lStructSize: uint32(unsafe.Sizeof(openFileNameW{})),
		hwndOwner:   owner,
		lpstrFilter: dialogFilter(filterDesc, filterPat),
		lpstrFile:   &file[0],
		nMaxFile:    uint32(len(file)),
		lpstrTitle:  utf16Ptr(title),
		flags:       ofnFileMustExist,
	}
	r, _, _ := procGetOpenFileNameW.Call(uintptr(unsafe.Pointer(&ofn)))
	if r == 0 {
		return ""
	}
	return windows.UTF16ToString(file[:])
}

// ---- 폴더 스캔/분류/순서 ----

type item struct {
	label   string
	path    string
	rel     string
	iconSrc string
	ord     int
}

// scan 은 데이터 폴더의 항목을 읽어 열별로 나눈다.
// 루트의 항목은 자동 분류, 열 하위 폴더 안의 항목은 그 열로 강제.
// 표시 순서는 .order 파일을 따르고, 새 항목은 뒤에 이름순으로 붙는다.
func (a *app) scan() [][]item {
	cats := make([][]item, len(a.cols))
	order := a.loadOrder()

	seen := a.loadSeen()
	seenChanged := false
	now := time.Now().Unix()
	present := map[string]bool{}

	appendItem := func(full string, isDir bool, ci int, iconSrc string) {
		rel, err := filepath.Rel(a.dataDir, full)
		if err != nil {
			rel = filepath.Base(full)
		}
		base := filepath.Base(full)
		present[base] = true
		first, ok := seen[base]
		if !ok {
			seen[base] = now
			first = now
			seenChanged = true
		}
		label := base
		if !isDir {
			label = strings.TrimSuffix(base, filepath.Ext(base))
		}
		if time.Duration(now-first)*time.Second < newBadgeAge {
			label += " !" // 신규 표시(오른쪽)
		}
		ord, ok := order[filepath.ToSlash(rel)]
		if !ok {
			ord = 1 << 30
		}
		cats[ci] = append(cats[ci], item{label: label, path: full, rel: rel, iconSrc: iconSrc, ord: ord})
	}

	// 열 강제 폴더
	for ci := range a.cols {
		dir := filepath.Join(a.dataDir, a.cols[ci].Folder)
		os.MkdirAll(dir, 0o755)
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if skipName(e.Name()) {
				continue
			}
			full := filepath.Join(dir, e.Name())
			_, iconSrc := a.classifyAuto(full, e.IsDir())
			appendItem(full, e.IsDir(), ci, iconSrc)
		}
	}

	// 루트: 자동 분류
	entries, err := os.ReadDir(a.dataDir)
	if err == nil {
		for _, e := range entries {
			name := e.Name()
			if skipName(name) {
				continue
			}
			if e.IsDir() && a.isColFolder(name) {
				continue
			}
			full := filepath.Join(a.dataDir, name)
			kind, iconSrc := a.classifyAuto(full, e.IsDir())
			appendItem(full, e.IsDir(), a.autoCol(kind), iconSrc)
		}
	}

	// 사라진 항목은 신규 기록에서 정리
	for name := range seen {
		if !present[name] {
			delete(seen, name)
			seenChanged = true
		}
	}
	if seenChanged {
		a.saveSeen(seen)
	}

	for ci := range cats {
		sort.SliceStable(cats[ci], func(i, j int) bool {
			if cats[ci][i].ord != cats[ci][j].ord {
				return cats[ci][i].ord < cats[ci][j].ord
			}
			return strings.ToLower(cats[ci][i].label) < strings.ToLower(cats[ci][j].label)
		})
	}
	return cats
}

func skipName(name string) bool {
	return strings.HasPrefix(name, ".") ||
		strings.EqualFold(name, "desktop.ini") ||
		strings.EqualFold(name, "config.json")
}

func (a *app) isColFolder(name string) bool {
	for _, c := range a.cols {
		if name == c.Folder {
			return true
		}
	}
	return false
}

// autoCol 은 자동 분류 종류("web"/"dev"/"etc")를 받을 열 번호를 정한다.
func (a *app) autoCol(kind string) int {
	for i, c := range a.cols {
		if c.Auto == kind {
			return i
		}
	}
	if kind != "etc" {
		for i, c := range a.cols {
			if c.Auto == "etc" {
				return i
			}
		}
	}
	return 0
}

// ---- 표시 순서(.order): dataDir 기준 상대 경로를 표시 순서대로 저장 ----

func (a *app) orderPath() string {
	return filepath.Join(a.dataDir, ".order")
}

func (a *app) loadOrder() map[string]int {
	m := map[string]int{}
	data, err := os.ReadFile(a.orderPath())
	if err != nil {
		return m
	}
	i := 0
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimRight(line, "\r"))
		if line == "" {
			continue
		}
		if _, dup := m[line]; !dup {
			m[line] = i
			i++
		}
	}
	return m
}

func (a *app) saveOrderFromItems() {
	var b strings.Builder
	for ci := range a.items {
		for _, it := range a.items[ci] {
			b.WriteString(filepath.ToSlash(it.rel))
			b.WriteString("\n")
		}
	}
	if err := os.WriteFile(a.orderPath(), []byte(b.String()), 0o644); err != nil {
		logErr(errFolder, ".order 저장 실패: "+err.Error())
	}
}

// ---- 신규(!) 기록: shortcuts\.seen 에 "파일이름<TAB>처음본시각" 저장 ----

func (a *app) seenPath() string {
	return filepath.Join(a.dataDir, ".seen")
}

func (a *app) loadSeen() map[string]int64 {
	m := map[string]int64{}
	data, err := os.ReadFile(a.seenPath())
	if err != nil {
		return m
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		i := strings.LastIndexByte(line, '\t')
		if i <= 0 {
			continue
		}
		ts, err := strconv.ParseInt(line[i+1:], 10, 64)
		if err != nil {
			continue
		}
		m[line[:i]] = ts
	}
	return m
}

func (a *app) saveSeen(m map[string]int64) {
	var b strings.Builder
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, "%s\t%d\n", n, m[n])
	}
	os.WriteFile(a.seenPath(), []byte(b.String()), 0o644)
}

// ---- 자동 분류 ----

// classifyAuto 는 항목의 자동 분류 종류("web"/"dev"/"etc")와 아이콘 소스 경로를 정한다.
func (a *app) classifyAuto(full string, isDir bool) (string, string) {
	if isDir {
		return a.textKind(full), full
	}
	switch strings.ToLower(filepath.Ext(full)) {
	case ".url":
		target := urlTarget(full)
		lower := strings.ToLower(target)
		if strings.HasPrefix(lower, "file:") {
			if local := fileURLToPath(target); local != "" {
				if _, err := os.Stat(local); err == nil {
					return a.textKind(full + " " + local), local
				}
			}
			return a.textKind(full), full
		}
		return "web", full
	case ".lnk":
		return a.textKind(full + " " + lnkTarget(full)), full
	default:
		return a.textKind(full), full
	}
}

func (a *app) textKind(s string) string {
	s = strings.ToLower(s)
	keys := a.devKeys
	if len(keys) == 0 {
		keys = defaultDevKeywords
	}
	for _, k := range keys {
		if k != "" && strings.Contains(s, strings.ToLower(k)) {
			return "dev"
		}
	}
	return "etc"
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

// ---- 업데이트 확인 ----

func checkUpdate(ch chan<- *updateInfo) {
	defer func() { recover() }()
	send := func(u *updateInfo) {
		select {
		case ch <- u:
		default:
		}
	}

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", repoOwner, repoName))
	if err != nil {
		logErr(errUpdate, "업데이트 확인 실패: "+err.Error())
		send(nil)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		logErr(errUpdate, fmt.Sprintf("업데이트 확인 실패: HTTP %d", resp.StatusCode))
		send(nil)
		return
	}

	var rel struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil || rel.TagName == "" {
		logErr(errUpdate, "업데이트 응답 해석 실패")
		send(nil)
		return
	}
	if !newerVersion(rel.TagName, version) {
		send(nil)
		return
	}
	url := rel.HTMLURL
	for _, as := range rel.Assets {
		if strings.EqualFold(as.Name, "gotool.exe") {
			url = as.URL
		}
	}
	send(&updateInfo{tag: rel.TagName, url: url})
}

// newerVersion 은 a("v1.2.3")가 b보다 새 버전이면 true.
func newerVersion(a, b string) bool {
	av, bv := verNums(a), verNums(b)
	for i := 0; i < 3; i++ {
		if av[i] != bv[i] {
			return av[i] > bv[i]
		}
	}
	return false
}

func verNums(v string) [3]int {
	var out [3]int
	v = strings.TrimLeft(strings.TrimSpace(v), "vV")
	for i, part := range strings.SplitN(v, ".", 3) {
		digits := part
		for j, c := range part {
			if c < '0' || c > '9' {
				digits = part[:j]
				break
			}
		}
		n, _ := strconv.Atoi(digits)
		out[i] = n
	}
	return out
}

// ---- 아이콘/폰트 ----

// iconHandle 은 경로의 셸 아이콘(HICON)을 얻는다. 쓰고 나면 DestroyIcon 필요.
func iconHandle(path string) windows.Handle {
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
	if r == 0 {
		return 0
	}
	return sfi.hIcon
}

func createUIFont(dpi int32) windows.Handle {
	height := -(9 * dpi) / 72 // 9pt
	face := utf16Ptr("맑은 고딕")
	f, _, _ := procCreateFontW.Call(
		uintptr(int(height)), 0, 0, 0,
		400, // FW_NORMAL
		0, 0, 0,
		1, // DEFAULT_CHARSET
		0, 0,
		5, // CLEARTYPE_QUALITY
		0,
		uintptr(unsafe.Pointer(face)),
	)
	return windows.Handle(f)
}

// ---- 추가(add) ----

// addItem 은 경로를 shortcuts 폴더에 추가한다.
// .lnk/.url 은 그대로 복사하고, 그 외 파일/폴더는 .lnk 바로가기를 만든다.
func addItem(src string) {
	src = strings.Trim(src, `"`)
	if abs, err := filepath.Abs(src); err == nil {
		src = abs
	}
	st, err := os.Stat(src)
	if err != nil {
		alertErr(errAdd, "경로를 찾을 수 없습니다:\n"+src, nil)
		return
	}

	dir, err := resolveDir(nil)
	if err != nil {
		alertErr(errFolder, err.Error(), nil)
		return
	}

	base := filepath.Base(src)
	ext := strings.ToLower(filepath.Ext(base))
	var dest string
	if !st.IsDir() && (ext == ".lnk" || ext == ".url") {
		dest = uniqueDest(dir, base)
		if err := copyFile(src, dest); err != nil {
			alertErr(errAdd, "복사하지 못했습니다", err)
			return
		}
	} else {
		name := base
		if !st.IsDir() {
			name = strings.TrimSuffix(base, filepath.Ext(base))
		}
		dest = uniqueDest(dir, name+".lnk")
		if err := createLnk(src, dest); err != nil {
			alertErr(errAdd, "바로가기를 만들지 못했습니다", err)
			return
		}
	}

	label := strings.TrimSuffix(filepath.Base(dest), filepath.Ext(dest))
	alert("gotool", fmt.Sprintf("추가되었습니다: %s\n\n(패널에서 항목을 드래그하면 원하는 열/순서로 옮길 수 있습니다)", label), mbIconInformation)
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

// ---- 오류 기록 ----

// logErr 는 exe 옆 gotool.log 에 "시각 <TAB> 버전 <TAB> 코드 <TAB> 내용" 형식으로 기록한다.
func logErr(code, msg string) {
	dir := "."
	if exe, err := os.Executable(); err == nil {
		dir = filepath.Dir(exe)
	}
	f, err := os.OpenFile(filepath.Join(dir, "gotool.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s\t%s\t%s\t%s\r\n", time.Now().Format("2006-01-02 15:04:05"), version, code, strings.ReplaceAll(msg, "\n", " "))
}

// alertErr 는 오류를 기록하고 코드와 함께 사용자에게 보여준다.
func alertErr(code, msg string, err error) {
	detail := msg
	if err != nil {
		detail += "\n" + err.Error()
	}
	logErr(code, detail)
	alert("gotool", fmt.Sprintf("[%s] %s", code, detail), mbIconError)
}

// ---- 공용 ----

func launch(path string) {
	op, _ := windows.UTF16PtrFromString("open")
	p, _ := windows.UTF16PtrFromString(path)
	procShellExecuteW.Call(0, uintptr(unsafe.Pointer(op)), uintptr(unsafe.Pointer(p)), 0, 0, swShowNormal)
}

func launchWith(exe, arg string) {
	op, _ := windows.UTF16PtrFromString("open")
	e, _ := windows.UTF16PtrFromString(exe)
	p, _ := windows.UTF16PtrFromString(`"` + arg + `"`)
	procShellExecuteW.Call(0, uintptr(unsafe.Pointer(op)), uintptr(unsafe.Pointer(e)), uintptr(unsafe.Pointer(p)), 0, swShowNormal)
}

func getSystemMetrics(index int32, fallback int32) int32 {
	r, _, _ := procGetSystemMetrics.Call(uintptr(index))
	if int32(r) == 0 && (index == smCxSmIcon || index == smCySmIcon || index == smCxVirtualScreen || index == smCyVirtualScreen) {
		return fallback
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
