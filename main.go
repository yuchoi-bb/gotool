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
	"bytes"
	"encoding/binary"
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
	"sync"
	"sync/atomic"
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
	errSelfUp   = "E10" // 자동 업데이트(자기 교체) 실패
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
	procLoadIconW           = user32.NewProc("LoadIconW")
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
	procCreateIconIndirect  = user32.NewProc("CreateIconIndirect")
	procGetDC               = user32.NewProc("GetDC")
	procReleaseDC           = user32.NewProc("ReleaseDC")
	procMonitorFromPoint    = user32.NewProc("MonitorFromPoint")
	procGetMonitorInfoW     = user32.NewProc("GetMonitorInfoW")
	procGetWindowRect       = user32.NewProc("GetWindowRect")
	procMoveWindow          = user32.NewProc("MoveWindow")
	procSendMessageW        = user32.NewProc("SendMessageW")
	procGetWindowTextW      = user32.NewProc("GetWindowTextW")
	procGetWindowTextLenW   = user32.NewProc("GetWindowTextLengthW")
	procSetWindowTextW      = user32.NewProc("SetWindowTextW")
	procAdjustWindowRectEx  = user32.NewProc("AdjustWindowRectEx")
	procIsDialogMessageW    = user32.NewProc("IsDialogMessageW")

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
	procCreateDIBSection       = gdi32.NewProc("CreateDIBSection")
	procCreateBitmap           = gdi32.NewProc("CreateBitmap")
	procGdiFlush               = gdi32.NewProc("GdiFlush")
	procCreateSolidBrush       = gdi32.NewProc("CreateSolidBrush")
	procRoundRect              = gdi32.NewProc("RoundRect")
	procGetStockObject         = gdi32.NewProc("GetStockObject")

	procSHGetFileInfoW = shell32.NewProc("SHGetFileInfoW")
	procShellExecuteW  = shell32.NewProc("ShellExecuteW")

	procCoInitializeEx   = ole32.NewProc("CoInitializeEx")
	procCoUninitialize   = ole32.NewProc("CoUninitialize")
	procCoCreateInstance = ole32.NewProc("CoCreateInstance")

	procGetModuleHandleW = kernel.NewProc("GetModuleHandleW")

	procGetSaveFileNameW = comdlg32.NewProc("GetSaveFileNameW")
	procGetOpenFileNameW = comdlg32.NewProc("GetOpenFileNameW")

	dwmapi                    = windows.NewLazySystemDLL("dwmapi.dll")
	procDwmSetWindowAttribute = dwmapi.NewProc("DwmSetWindowAttribute")
)

const (
	wsPopup  = 0x80000000
	wsBorder = 0x00800000

	wsExTopmost    = 0x00000008
	wsExToolWindow = 0x00000080

	swShow = 5

	swpNoSize   = 0x0001
	swpNoMove   = 0x0002
	swpNoZorder = 0x0004

	wsChild   = 0x40000000
	wsVisible = 0x10000000
	wsCaption = 0x00C00000
	wsSysMenu = 0x00080000
	wsTabStop = 0x00010000

	esAutoHScroll   = 0x0080
	bsDefPushButton = 0x0001

	wmClose   = 0x0010
	wmSetFont = 0x0030
	wmCommand = 0x0111

	monitorDefaultToNearest = 2

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
	wmAppUpdated  = 0x8000 + 2 // 자동 업데이트(다운로드/교체) 완료
	wmAppIcon     = 0x8000 + 3 // 백그라운드 아이콘 추출 결과 도착

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

	nullPenStock = 8 // NULL_PEN

	dwmwaCornerPreference = 33 // DWMWA_WINDOW_CORNER_PREFERENCE (Win11)
	dwmwcpRound           = 2  // DWMWCP_ROUND
)

// ---- 색상 팔레트(라이트/다크 자동) ----

type palette struct {
	bg         uint32 // 배경
	text       uint32 // 본문
	subtle     uint32 // 흐린 텍스트(비어 있음, 진행 중 등)
	hover      uint32 // 마우스 오버 배경
	hoverText  uint32 // 마우스 오버 텍스트
	border     uint32 // 창 테두리
	sep        uint32 // 구분선
	accent     uint32 // 포인트 색(열 제목, 업데이트, 드롭 표시)
	accentSoft uint32 // 포인트 색 연한 배경
}

func rgbRef(r, g, b uint8) uint32 { return uint32(r) | uint32(g)<<8 | uint32(b)<<16 }

// systemDarkMode 는 Windows 앱 테마가 다크인지 확인한다.
func systemDarkMode() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Themes\Personalize`, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetIntegerValue("AppsUseLightTheme")
	return err == nil && v == 0
}

func loadPalette() palette {
	if systemDarkMode() {
		return palette{
			bg:         rgbRef(0x2B, 0x2B, 0x30),
			text:       rgbRef(0xF2, 0xF2, 0xF5),
			subtle:     rgbRef(0xA6, 0xA6, 0xB0),
			hover:      rgbRef(0x41, 0x3C, 0x50),
			hoverText:  rgbRef(0xFF, 0xFF, 0xFF),
			border:     rgbRef(0x4A, 0x4A, 0x52),
			sep:        rgbRef(0x3E, 0x3E, 0x46),
			accent:     rgbRef(0xB9, 0x9A, 0xF8),
			accentSoft: rgbRef(0x45, 0x3B, 0x5E),
		}
	}
	return palette{
		bg:         rgbRef(0xFC, 0xFC, 0xFE),
		text:       rgbRef(0x25, 0x25, 0x2A),
		subtle:     rgbRef(0x8C, 0x8C, 0x96),
		hover:      rgbRef(0xEF, 0xE9, 0xFC),
		hoverText:  rgbRef(0x33, 0x1D, 0x6E),
		border:     rgbRef(0xE0, 0xE0, 0xE8),
		sep:        rgbRef(0xEC, 0xEC, 0xF2),
		accent:     rgbRef(0x7C, 0x3A, 0xED),
		accentSoft: rgbRef(0xF2, 0xEC, 0xFE),
	}
}

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

type monitorInfo struct {
	cbSize    uint32
	rcMonitor rect
	rcWork    rect
	dwFlags   uint32
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

type iconInfoStruct struct {
	fIcon    int32
	xHotspot uint32
	yHotspot uint32
	hbmMask  windows.Handle
	hbmColor windows.Handle
}

// monitorWorkFromPoint 는 해당 좌표가 속한 모니터의 작업 영역을 돌려준다.
// 창이 모니터 경계를 넘어 옆 모니터로 걸치지 않게 하는 데 쓴다.
func monitorWorkFromPoint(pt point) rect {
	packed := uintptr(uint32(pt.x)) | uintptr(uint32(pt.y))<<32
	hm, _, _ := procMonitorFromPoint.Call(packed, monitorDefaultToNearest)
	if hm != 0 {
		mi := monitorInfo{cbSize: uint32(unsafe.Sizeof(monitorInfo{}))}
		if r, _, _ := procGetMonitorInfoW.Call(hm, uintptr(unsafe.Pointer(&mi))); r != 0 {
			return mi.rcWork
		}
	}
	return rect{0, 0, getSystemMetrics(smCxVirtualScreen, 1920), getSystemMetrics(smCyVirtualScreen, 1080)}
}

// clampIntoWork 는 (x,y,w,h) 창이 작업 영역 안에 들어오도록 좌표를 보정한다.
func clampIntoWork(wa rect, x, y, w, h int32) (int32, int32) {
	if x+w > wa.right {
		x = wa.right - w
	}
	if y+h > wa.bottom {
		y = wa.bottom - h
	}
	if x < wa.left {
		x = wa.left
	}
	if y < wa.top {
		y = wa.top
	}
	return x, y
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
	dataDir  string
	cols     []column
	devKeys  []string
	hwnd     windows.Handle
	font     windows.Handle
	fontBold windows.Handle
	pal      palette
	iconCx   int32
	iconCy   int32
	dpi      int32

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

	modal bool // 대화상자/설정 창 표시 중(비활성화로 창이 닫히지 않게)

	update   *updateInfo
	updateCh chan *updateInfo
	updating bool // 자동 업데이트 다운로드 진행 중
	updErrCh chan error

	settings   *settingsUI // 열려 있는 설정 창(없으면 nil)
	setClsReg  bool
	panelAlive bool

	// 아이콘 비동기 로딩/캐시: 패널을 먼저 띄우고 아이콘은 뒤에서 채운다.
	gen       int64 // reload 세대(이전 워커 결과 무시용, atomic)
	iconCh    chan iconResult
	cacheMu   sync.Mutex
	iconCache map[string]cacheEnt
}

type iconResult struct {
	gen  int64
	cat  int
	idx  int
	icon windows.Handle
}

type iconJob struct {
	cat   int
	idx   int
	src   string
	key   string
	mtime int64
}

type cacheEnt struct {
	mtime int64
	w, h  int32
	pix   []byte // RGBA
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

	// 지난 자동 업데이트가 남긴 이전 버전 파일 정리(실패해도 무시)
	if exe, err := os.Executable(); err == nil {
		os.Remove(filepath.Join(filepath.Dir(exe), "gotool-old.exe"))
	}

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
	t0 := time.Now()
	a := &app{
		dataDir:  dir,
		iconCx:   getSystemMetrics(smCxSmIcon, 16),
		iconCy:   getSystemMetrics(smCySmIcon, 16),
		updateCh: make(chan *updateInfo, 1),
		updErrCh: make(chan error, 1),
		iconCh:   make(chan iconResult, 1024),
		hover:    hit{kind: hitNone},
		pressed:  hit{kind: hitNone},
		dropCat:  -1,
		dropIdx:  -1,
	}
	a.iconCache = loadIconCache(a.iconCachePath())

	hdcScreen, _, _ := procGetDC.Call(0)
	dpi, _, _ := procGetDeviceCaps.Call(hdcScreen, logPixelsY)
	procReleaseDC.Call(0, hdcScreen)
	a.dpi = int32(dpi)
	if a.dpi <= 0 {
		a.dpi = 96
	}
	a.pal = loadPalette()
	a.font = createUIFont(a.dpi, 400)
	a.fontBold = createUIFont(a.dpi, 600)
	defer procDeleteObject.Call(uintptr(a.font))
	defer procDeleteObject.Call(uintptr(a.fontBold))

	if !a.createWindow() {
		alertErr(errWindow, "창을 생성하지 못했습니다", nil)
		return
	}

	tReload := time.Now()
	a.reload()
	reloadMs := time.Since(tReload).Milliseconds()

	// 커서 위치에 표시. 커서가 있는 모니터의 작업 영역 안에서만 열리게 보정
	// (모니터 가장자리라도 옆 모니터로 넘어가지 않음)
	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	x, y := clampIntoWork(monitorWorkFromPoint(pt), pt.x, pt.y, a.winW, a.winH)
	procSetWindowPos.Call(uintptr(a.hwnd), 0, uintptr(x), uintptr(y), uintptr(a.winW), uintptr(a.winH), swpNoZorder)
	procShowWindow.Call(uintptr(a.hwnd), swShow)
	procSetForegroundWindow.Call(uintptr(a.hwnd))

	// 시작 성능 프로파일: 내부 처리가 느리면 원인 파악용으로 기록한다.
	// 여기 수치가 작은데도 실행이 느리게 느껴지면 원인은 앱 바깥(Defender/SmartScreen의
	// exe 검사, 디스크 캐시 미적재 등)이다.
	if total := time.Since(t0).Milliseconds(); total > 300 {
		logErr("P01", fmt.Sprintf("느린 시작: 창 표시까지 내부 처리 %dms (폴더 스캔·아이콘 복원 %dms)", total, reloadMs))
	}

	// 업데이트 확인은 백그라운드로. 끝나면 UI 스레드에 알림.
	go func() {
		checkUpdate(a.updateCh)
		procPostMessageW.Call(uintptr(a.hwnd), wmAppUpdate, 0, 0)
	}()

	a.panelAlive = true

	var m msgStruct
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if r == 0 || int32(r) == -1 {
			break
		}
		// 설정 창이 열려 있으면 TAB/Enter/ESC 등 대화상자 키 처리
		if a.settings != nil && a.settings.hwnd != 0 {
			if d, _, _ := procIsDialogMessageW.Call(uintptr(a.settings.hwnd), uintptr(unsafe.Pointer(&m))); d != 0 {
				continue
			}
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

	appIcon, _, _ := procLoadIconW.Call(hInst, 1) // rsrc 로 임베드한 앱 아이콘

	wndProc := windows.NewCallback(a.wndProc)
	wc := wndClassEx{
		cbSize:        uint32(unsafe.Sizeof(wndClassEx{})),
		lpfnWndProc:   wndProc,
		hInstance:     windows.Handle(hInst),
		hIcon:         windows.Handle(appIcon),
		hIconSm:       windows.Handle(appIcon),
		hCursor:       windows.Handle(arrow),
		lpszClassName: className,
	}
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	hwnd, _, _ := procCreateWindowExW.Call(
		wsExTopmost|wsExToolWindow,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(utf16Ptr("gotool"))),
		wsPopup, // 테두리는 팔레트 색으로 직접 그린다
		0, 0, 100, 100,
		0, 0, hInst, 0,
	)
	a.hwnd = windows.Handle(hwnd)
	if hwnd != 0 {
		// Windows 11: 둥근 모서리 (Win10 이하에서는 조용히 무시됨)
		pref := uint32(dwmwcpRound)
		procDwmSetWindowAttribute.Call(hwnd, dwmwaCornerPreference, uintptr(unsafe.Pointer(&pref)), 4)
	}
	return hwnd != 0
}

// rowH 는 항목/버튼 한 줄의 높이(여유 있는 클릭 영역).
func (a *app) rowH() int32 { return a.iconCy + a.scale(14) }

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
		if wparam == vkEscape && !a.updating {
			procDestroyWindow.Call(hwnd)
			return 0
		}
	case wmActivate:
		if wparam&0xFFFF == waInactive && !a.modal && !a.dragging && !a.updating {
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
	case wmAppIcon:
		a.drainIcons()
		return 0
	case wmAppUpdated:
		var err error
		select {
		case err = <-a.updErrCh:
		default:
		}
		a.updating = false
		if err != nil {
			// 자동 교체 실패(권한 등) 시 기존 방식(브라우저 다운로드)으로 대체
			a.modalAlertErr(errSelfUp, "자동 업데이트에 실패해 브라우저로 내려받습니다.\n다운로드한 파일로 gotool.exe를 직접 바꿔 주세요", err)
			if a.update != nil {
				launch(a.update.url)
			}
			procInvalidateRect.Call(uintptr(a.hwnd), 0, 1)
			return 0
		}
		a.modal = true
		alert("gotool", "업데이트가 완료되었습니다!\n새 버전을 실행합니다.", mbIconInformation)
		a.modal = false
		if exe, e := os.Executable(); e == nil {
			launch(exe)
		}
		procDestroyWindow.Call(uintptr(a.hwnd))
		return 0
	case wmDestroy:
		a.panelAlive = false
		if a.settings == nil {
			procPostQuitMessage.Call(0)
		}
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

	// 패널을 빨리 띄우기 위해 아이콘은 여기서 추출하지 않는다:
	// 캐시에 있으면 즉시 복원, 없으면 백그라운드 워커가 추출해서 채운다.
	gen := atomic.AddInt64(&a.gen, 1)
	var jobs []iconJob
	var live []string

	cats := a.scan()
	a.items = make([][]uiItem, len(a.cols))
	for ci := range a.cols {
		for _, it := range cats[ci] {
			key := filepath.ToSlash(it.rel) + "|" + strconv.Itoa(int(a.iconCx))
			live = append(live, key)
			var icon windows.Handle
			a.cacheMu.Lock()
			if ce, ok := a.iconCache[key]; ok && ce.mtime == it.mtime {
				icon = iconFromRGBA(ce.w, ce.h, ce.pix)
			}
			a.cacheMu.Unlock()
			if icon == 0 {
				jobs = append(jobs, iconJob{cat: ci, idx: len(a.items[ci]), src: it.iconSrc, key: key, mtime: it.mtime})
			}
			a.items[ci] = append(a.items[ci], uiItem{
				label: it.label,
				path:  it.path,
				rel:   it.rel,
				icon:  icon,
			})
		}
	}
	a.saveOrderFromItems()
	a.layout()

	if len(jobs) > 0 {
		go a.iconWorker(gen, jobs, live)
	}
}

// iconWorker 는 백그라운드에서 셸 아이콘을 추출해 UI 스레드로 보내고,
// 추출한 픽셀을 디스크 캐시(.iconcache)에 저장해 다음 실행부터 즉시 표시되게 한다.
func (a *app) iconWorker(gen int64, jobs []iconJob, live []string) {
	defer func() { recover() }()
	runtime.LockOSThread()
	procCoInitializeEx.Call(0, coinitApartment)
	defer procCoUninitialize.Call()

	fresh := map[string]cacheEnt{}
	for _, j := range jobs {
		if atomic.LoadInt64(&a.gen) != gen {
			return // 그 사이 새로고침됨 → 중단
		}
		icon := iconHandle(j.src)
		if icon != 0 {
			if pix := hiconToRGBA(icon, a.iconCx, a.iconCy); pix != nil {
				fresh[j.key] = cacheEnt{mtime: j.mtime, w: a.iconCx, h: a.iconCy, pix: pix}
			}
		}
		select {
		case a.iconCh <- iconResult{gen: gen, cat: j.cat, idx: j.idx, icon: icon}:
			procPostMessageW.Call(uintptr(a.hwnd), wmAppIcon, 0, 0)
		default:
			if icon != 0 {
				procDestroyIcon.Call(uintptr(icon))
			}
		}
	}

	// 캐시 병합: 새 항목 반영 + 사라진 항목 정리 후 저장
	liveSet := map[string]bool{}
	for _, k := range live {
		liveSet[k] = true
	}
	a.cacheMu.Lock()
	for k, v := range fresh {
		a.iconCache[k] = v
	}
	for k := range a.iconCache {
		if !liveSet[k] {
			delete(a.iconCache, k)
		}
	}
	saveIconCache(a.iconCachePath(), a.iconCache)
	a.cacheMu.Unlock()
}

// drainIcons 는 워커가 보낸 아이콘을 항목에 반영한다(세대가 다르면 폐기).
func (a *app) drainIcons() {
	changed := false
	for {
		select {
		case r := <-a.iconCh:
			if r.gen == atomic.LoadInt64(&a.gen) &&
				r.cat < len(a.items) && r.idx < len(a.items[r.cat]) &&
				a.items[r.cat][r.idx].icon == 0 {
				a.items[r.cat][r.idx].icon = r.icon
				changed = true
			} else if r.icon != 0 {
				procDestroyIcon.Call(uintptr(r.icon))
			}
		default:
			if changed {
				procInvalidateRect.Call(uintptr(a.hwnd), 0, 0)
			}
			return
		}
	}
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

	pad := a.scale(10)
	rowH := a.rowH()
	sepW := a.scale(13)
	iconPad := a.scale(8)
	minCol := a.scale(140)
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
		w += a.iconCx + iconPad + pad*2 + a.scale(8)
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
	y := itemsBottom + a.scale(8)
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

	pal := a.pal
	brushes := map[uint32]uintptr{}
	brush := func(c uint32) uintptr {
		if b, ok := brushes[c]; ok {
			return b
		}
		b, _, _ := procCreateSolidBrush.Call(uintptr(c))
		brushes[c] = b
		return b
	}
	defer func() {
		for _, b := range brushes {
			procDeleteObject.Call(b)
		}
	}()
	nullPen, _, _ := procGetStockObject.Call(nullPenStock)

	// 둥근 채우기(마우스 오버 등)
	fillRound := func(r rect, c uint32, rad int32) {
		oldB, _, _ := procSelectObject.Call(memDC, brush(c))
		oldP, _, _ := procSelectObject.Call(memDC, nullPen)
		procRoundRect.Call(memDC, uintptr(r.left), uintptr(r.top), uintptr(r.right)+1, uintptr(r.bottom)+1, uintptr(rad), uintptr(rad))
		procSelectObject.Call(memDC, oldB)
		procSelectObject.Call(memDC, oldP)
	}

	procFillRect.Call(memDC, uintptr(unsafe.Pointer(&rc)), brush(pal.bg))

	drawText := func(r rect, s string, col uint32) {
		u, err := windows.UTF16FromString(s)
		if err != nil {
			return
		}
		procSetTextColor.Call(memDC, uintptr(col))
		procDrawTextW.Call(memDC, uintptr(unsafe.Pointer(&u[0])), ^uintptr(0), uintptr(unsafe.Pointer(&r)), dtFlags)
	}

	rad := a.scale(6)
	rowText := func(r rect, label string, icon windows.Handle, hovered, grayed bool, hoverFill, textCol uint32) {
		if hovered {
			hr := rect{r.left + a.scale(2), r.top + a.scale(1), r.right - a.scale(2), r.bottom - a.scale(1)}
			fillRound(hr, hoverFill, rad)
		}
		x := r.left + a.scale(10)
		if icon != 0 {
			iy := r.top + (r.bottom-r.top-a.iconCy)/2
			procDrawIconEx.Call(memDC, uintptr(x), uintptr(iy), uintptr(icon), uintptr(a.iconCx), uintptr(a.iconCy), 0, 0, diNormal)
		}
		tr := r
		tr.left = x + a.iconCx + a.scale(8)
		col := textCol
		if grayed {
			col = pal.subtle
		} else if hovered {
			col = pal.hoverText
		}
		drawText(tr, label, col)
	}

	// 업데이트 버튼(포인트 색으로 강조)
	if a.update != nil {
		label := "⬆ 새 버전 " + a.update.tag + " 업데이트 — 클릭 한 번으로 자동 교체"
		if a.updating {
			label = "⬇ " + a.update.tag + " 다운로드 중... 잠시만 기다려 주세요"
		}
		hovered := a.hover.kind == hitUpdate && !a.updating
		fillRound(rect{a.updRc.left, a.updRc.top, a.updRc.right, a.updRc.bottom}, pal.accentSoft, rad)
		if hovered {
			fillRound(rect{a.updRc.left, a.updRc.top, a.updRc.right, a.updRc.bottom}, pal.hover, rad)
		}
		tr := a.updRc
		tr.left += a.scale(12)
		if a.updating {
			drawText(tr, label, pal.subtle)
		} else {
			drawText(tr, label, pal.accent)
		}
	}

	// 열
	for ci := range a.cols {
		hr := a.headerRc[ci]
		procSelectObject.Call(memDC, uintptr(a.fontBold))
		drawText(rect{hr.left + a.scale(10), hr.top, hr.right, hr.bottom}, a.cols[ci].Name, pal.accent)
		procSelectObject.Call(memDC, uintptr(a.font))
		line := rect{hr.left + a.scale(2), hr.bottom - a.scale(2), hr.right - a.scale(2), hr.bottom - a.scale(1)}
		procFillRect.Call(memDC, uintptr(unsafe.Pointer(&line)), brush(pal.sep))

		if len(a.items[ci]) == 0 {
			r := rect{hr.left, hr.bottom, hr.right, hr.bottom + a.rowH()}
			drawText(rect{r.left + a.scale(10), r.top, r.right, r.bottom}, "(비어 있음)", pal.subtle)
		}
		for i, it := range a.items[ci] {
			hovered := !a.dragging && a.hover.kind == hitItem && a.hover.cat == ci && a.hover.idx == i
			graying := a.dragging && a.pressed.kind == hitItem && a.pressed.cat == ci && a.pressed.idx == i
			rowText(it.rc, it.label, it.icon, hovered, graying, pal.hover, pal.text)
		}
		if ci < len(a.cols)-1 {
			sx := a.colBand[ci].right + a.scale(6)
			line := rect{sx, a.colBand[ci].top + a.scale(2), sx + a.scale(1), a.colBand[ci].bottom - a.scale(2)}
			procFillRect.Call(memDC, uintptr(unsafe.Pointer(&line)), brush(pal.sep))
		}
	}

	// 드래그 중: 드롭 대상 열 강조 + 삽입 위치 표시
	if a.dragging && a.dropCat >= 0 {
		band := a.colBand[a.dropCat]
		procFrameRect.Call(memDC, uintptr(unsafe.Pointer(&band)), brush(pal.accent))
		inner := rect{band.left + 1, band.top + 1, band.right - 1, band.bottom - 1}
		procFrameRect.Call(memDC, uintptr(unsafe.Pointer(&inner)), brush(pal.accent))

		if a.dropIdx >= 0 {
			ly := a.headerRc[a.dropCat].bottom + int32(a.dropIdx)*a.rowH()
			mark := rect{band.left + a.scale(4), ly - a.scale(1), band.right - a.scale(4), ly + a.scale(2)}
			fillRound(mark, pal.accent, a.scale(2))
		}
	}

	// 하단 관리 버튼
	if len(a.buttons) > 0 {
		top := a.buttons[0].rc.top
		line := rect{a.buttons[0].rc.left + a.scale(2), top - a.scale(4), a.buttons[0].rc.right - a.scale(2), top - a.scale(3)}
		procFillRect.Call(memDC, uintptr(unsafe.Pointer(&line)), brush(pal.sep))
	}
	for _, b := range a.buttons {
		rowText(b.rc, b.label, 0, a.hover.kind == b.kind, false, pal.hover, pal.subtle)
	}

	// 창 테두리
	procFrameRect.Call(memDC, uintptr(unsafe.Pointer(&rc)), brush(pal.border))

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
				rowH := a.rowH()
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
		// 브라우저로 보내지 않고 직접 내려받아 자기 자신을 교체한다.
		if a.update == nil || a.updating {
			return
		}
		a.updating = true
		procInvalidateRect.Call(uintptr(a.hwnd), 0, 0)
		url := a.update.url
		hwnd := a.hwnd
		go func() {
			err := selfUpdate(url)
			select {
			case a.updErrCh <- err:
			default:
			}
			procPostMessageW.Call(uintptr(hwnd), wmAppUpdated, 0, 0)
		}()
	case hitSettings:
		a.openSettings()
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

// ---- 설정 창 (열 이름·구성 편집 UI) ----

// 대화상자 키 처리(IsDialogMessage)와 맞추기 위해 저장=IDOK(1), 취소=IDCANCEL(2)을 쓴다.
const (
	idSetSave   = 1 // Enter 키로도 저장
	idSetCancel = 2 // ESC 키로도 닫기
	idSetAdd    = 3
	idSetRow    = 100 // 행 컨트롤 공용 ID(실제 구분은 핸들로)
)

var autoCycle = []string{"", "web", "dev", "etc"}

func autoLabel(auto string) string {
	switch auto {
	case "web":
		return "자동: 웹주소"
	case "dev":
		return "자동: 개발"
	case "etc":
		return "자동: 기타"
	}
	return "자동: 없음"
}

type setRow struct {
	nameEd   windows.Handle
	folderEd windows.Handle
	autoBtn  windows.Handle
	delBtn   windows.Handle
	auto     string
}

type settingsUI struct {
	a       *app
	hwnd    windows.Handle
	rows    []*setRow
	addBtn  windows.Handle
	saveBtn windows.Handle
	cancel  windows.Handle
	devKeys []string
}

const settingsStyle = wsPopup | wsCaption | wsSysMenu

func (a *app) openSettings() {
	if a.settings != nil {
		procSetForegroundWindow.Call(uintptr(a.settings.hwnd))
		return
	}
	a.modal = true // 설정 창이 떠 있는 동안 패널이 닫히지 않게
	s := &settingsUI{a: a}
	a.settings = s
	if !s.create() {
		a.settings = nil
		a.modal = false
		a.modalAlertErr(errWindow, "설정 창을 만들지 못했습니다", nil)
	}
}

func (s *settingsUI) create() bool {
	a := s.a
	hInst, _, _ := procGetModuleHandleW.Call(0)
	className := utf16Ptr("gotoolSettingsWnd")

	if !a.setClsReg {
		arrow, _, _ := procLoadCursorW.Call(0, idcArrow)
		appIcon, _, _ := procLoadIconW.Call(hInst, 1)
		wndProc := windows.NewCallback(a.settingsProc)
		wc := wndClassEx{
			cbSize:        uint32(unsafe.Sizeof(wndClassEx{})),
			lpfnWndProc:   wndProc,
			hInstance:     windows.Handle(hInst),
			hIcon:         windows.Handle(appIcon),
			hIconSm:       windows.Handle(appIcon),
			hCursor:       windows.Handle(arrow),
			hbrBackground: windows.Handle(colorMenu + 1),
			lpszClassName: className,
		}
		procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
		a.setClsReg = true
	}

	hwnd, _, _ := procCreateWindowExW.Call(
		wsExTopmost|wsExToolWindow,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(utf16Ptr("gotool 설정 — 열 구성"))),
		settingsStyle,
		0, 0, 10, 10,
		0, 0, hInst, 0,
	)
	if hwnd == 0 {
		return false
	}
	s.hwnd = windows.Handle(hwnd)

	cfg := loadConfig(a.dataDir)
	s.devKeys = cfg.DevKeywords

	// 머리글
	s.static("열 이름")
	s.static("폴더 이름")
	s.static("자동 분류")
	for _, c := range cfg.Columns {
		s.addRowControls(c)
	}
	s.addBtn = s.child("BUTTON", "＋ 열 추가", wsChild|wsVisible|wsTabStop, idSetAdd)
	s.saveBtn = s.child("BUTTON", "저장", wsChild|wsVisible|wsTabStop|bsDefPushButton, idSetSave)
	s.cancel = s.child("BUTTON", "취소", wsChild|wsVisible|wsTabStop, idSetCancel)

	s.relayout()

	// 패널 근처, 같은 모니터 안에 표시
	var pr rect
	procGetWindowRect.Call(uintptr(a.hwnd), uintptr(unsafe.Pointer(&pr)))
	var wr rect
	procGetWindowRect.Call(uintptr(s.hwnd), uintptr(unsafe.Pointer(&wr)))
	w, h := wr.right-wr.left, wr.bottom-wr.top
	x, y := clampIntoWork(monitorWorkFromPoint(point{pr.left, pr.top}), pr.left+a.scale(24), pr.top+a.scale(24), w, h)
	procSetWindowPos.Call(uintptr(s.hwnd), 0, uintptr(x), uintptr(y), 0, 0, swpNoSize|swpNoZorder)

	procShowWindow.Call(uintptr(s.hwnd), swShow)
	procSetForegroundWindow.Call(uintptr(s.hwnd))
	return true
}

// static 은 머리글 라벨을 만든다(위치는 relayout에서).
func (s *settingsUI) static(text string) windows.Handle {
	return s.child("STATIC", text, wsChild|wsVisible, idSetRow)
}

var settingsLabels []windows.Handle // 머리글 3개(단일 설정 창 전제)

func (s *settingsUI) child(class, text string, style uintptr, id int) windows.Handle {
	hInst, _, _ := procGetModuleHandleW.Call(0)
	h, _, _ := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(utf16Ptr(class))),
		uintptr(unsafe.Pointer(utf16Ptr(text))),
		style,
		0, 0, 10, 10,
		uintptr(s.hwnd), uintptr(id), hInst, 0,
	)
	procSendMessageW.Call(h, wmSetFont, uintptr(s.a.font), 1)
	if class == "STATIC" {
		settingsLabels = append(settingsLabels, windows.Handle(h))
	}
	return windows.Handle(h)
}

func (s *settingsUI) addRowControls(c column) {
	r := &setRow{auto: c.Auto}
	r.nameEd = s.child("EDIT", c.Name, wsChild|wsVisible|wsBorder|wsTabStop|esAutoHScroll, idSetRow)
	r.folderEd = s.child("EDIT", c.Folder, wsChild|wsVisible|wsBorder|wsTabStop|esAutoHScroll, idSetRow)
	r.autoBtn = s.child("BUTTON", autoLabel(c.Auto), wsChild|wsVisible|wsTabStop, idSetRow)
	r.delBtn = s.child("BUTTON", "✕", wsChild|wsVisible|wsTabStop, idSetRow)
	s.rows = append(s.rows, r)
}

// relayout 은 컨트롤 배치와 창 크기를 다시 계산한다.
func (s *settingsUI) relayout() {
	a := s.a
	m := a.scale(12)
	rh := a.scale(32)
	eh := a.scale(24)
	gap := a.scale(8)
	wName, wFolder, wAuto, wDel := a.scale(190), a.scale(130), a.scale(120), a.scale(30)

	move := func(h windows.Handle, x, y, w, hh int32) {
		procMoveWindow.Call(uintptr(h), uintptr(x), uintptr(y), uintptr(w), uintptr(hh), 1)
	}

	x0 := m
	x1 := x0 + wName + gap
	x2 := x1 + wFolder + gap
	x3 := x2 + wAuto + gap
	clientW := x3 + wDel + m

	// 머리글
	if len(settingsLabels) >= 3 {
		move(settingsLabels[0], x0, m, wName, eh)
		move(settingsLabels[1], x1, m, wFolder, eh)
		move(settingsLabels[2], x2, m, wAuto, eh)
	}

	y := m + a.scale(24)
	for _, r := range s.rows {
		move(r.nameEd, x0, y, wName, eh)
		move(r.folderEd, x1, y, wFolder, eh)
		move(r.autoBtn, x2, y, wAuto, eh)
		move(r.delBtn, x3, y, wDel, eh)
		y += rh
	}
	y += a.scale(8)

	bh := a.scale(28)
	bw := a.scale(96)
	move(s.addBtn, m, y, a.scale(110), bh)
	move(s.saveBtn, clientW-m-bw*2-gap, y, bw, bh)
	move(s.cancel, clientW-m-bw, y, bw, bh)
	y += bh + m

	rc := rect{0, 0, clientW, y}
	procAdjustWindowRectEx.Call(uintptr(unsafe.Pointer(&rc)), settingsStyle, 0, wsExTopmost|wsExToolWindow)
	procSetWindowPos.Call(uintptr(s.hwnd), 0, 0, 0, uintptr(rc.right-rc.left), uintptr(rc.bottom-rc.top), swpNoMove|swpNoZorder)
	procInvalidateRect.Call(uintptr(s.hwnd), 0, 1)
}

func (a *app) settingsProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	s := a.settings
	switch msg {
	case wmCommand:
		if s == nil {
			break
		}
		switch int(wparam & 0xFFFF) {
		case idSetSave:
			if s.save() {
				procDestroyWindow.Call(hwnd)
			}
			return 0
		case idSetCancel:
			procDestroyWindow.Call(hwnd)
			return 0
		case idSetAdd:
			s.addRowControls(column{Name: "", Folder: ""})
			s.relayout()
			return 0
		}
		ctrl := windows.Handle(lparam)
		for i, r := range s.rows {
			if ctrl == r.autoBtn {
				next := 0
				for j, k := range autoCycle {
					if k == r.auto {
						next = (j + 1) % len(autoCycle)
					}
				}
				r.auto = autoCycle[next]
				procSetWindowTextW.Call(uintptr(r.autoBtn), uintptr(unsafe.Pointer(utf16Ptr(autoLabel(r.auto)))))
				return 0
			}
			if ctrl == r.delBtn {
				if len(s.rows) <= 1 {
					s.warn("열은 최소 1개가 필요합니다.")
					return 0
				}
				for _, h := range []windows.Handle{r.nameEd, r.folderEd, r.autoBtn, r.delBtn} {
					procDestroyWindow.Call(uintptr(h))
				}
				s.rows = append(s.rows[:i], s.rows[i+1:]...)
				s.relayout()
				return 0
			}
		}
		return 0
	case wmClose:
		procDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		a.settings = nil
		settingsLabels = nil
		a.modal = false
		if a.panelAlive {
			a.refresh()
			procSetForegroundWindow.Call(uintptr(a.hwnd))
		} else {
			procPostQuitMessage.Call(0)
		}
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
	return r
}

func (s *settingsUI) warn(msg string) {
	procMessageBoxW.Call(uintptr(s.hwnd),
		uintptr(unsafe.Pointer(utf16Ptr(msg))),
		uintptr(unsafe.Pointer(utf16Ptr("gotool 설정"))),
		mbOK|mbIconWarning)
}

// save 는 입력을 검증해 config.json 에 저장한다. 실패하면 창을 닫지 않는다.
func (s *settingsUI) save() bool {
	var cols []column
	used := map[string]bool{}
	for _, r := range s.rows {
		name := strings.TrimSpace(getWindowText(r.nameEd))
		folder := strings.TrimSpace(getWindowText(r.folderEd))
		if folder == "" {
			s.warn("폴더 이름을 입력해 주세요. (shortcuts 안에 만들어질 하위 폴더 이름)")
			return false
		}
		if strings.ContainsAny(folder, `\/:*?"<>|`) {
			s.warn("폴더 이름에 사용할 수 없는 문자가 있습니다:\n" + folder)
			return false
		}
		lf := strings.ToLower(folder)
		if used[lf] {
			s.warn("폴더 이름이 중복되었습니다:\n" + folder)
			return false
		}
		used[lf] = true
		if name == "" {
			name = folder
		}
		cols = append(cols, column{Name: name, Folder: folder, Auto: r.auto})
	}
	if len(cols) == 0 {
		s.warn("열은 최소 1개가 필요합니다.")
		return false
	}
	saveConfig(s.a.dataDir, appConfig{Columns: cols, DevKeywords: s.devKeys})
	return true
}

func getWindowText(h windows.Handle) string {
	n, _, _ := procGetWindowTextLenW.Call(uintptr(h))
	if n == 0 {
		return ""
	}
	buf := make([]uint16, n+1)
	procGetWindowTextW.Call(uintptr(h), uintptr(unsafe.Pointer(&buf[0])), n+1)
	return windows.UTF16ToString(buf)
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
	mtime   int64
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

	appendItem := func(e fs.DirEntry, full string, isDir bool, ci int, iconSrc string) {
		rel, err := filepath.Rel(a.dataDir, full)
		if err != nil {
			rel = filepath.Base(full)
		}
		var mtime int64
		if fi, err := e.Info(); err == nil {
			mtime = fi.ModTime().Unix()
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
		cats[ci] = append(cats[ci], item{label: label, path: full, rel: rel, iconSrc: iconSrc, ord: ord, mtime: mtime})
	}

	// 분류 결과 캐시(.meta): .lnk/.url 분류는 대상 경로 확인(os.Stat)이 필요해
	// 대상이 끊긴 네트워크 드라이브면 수 초씩 걸릴 수 있다. mtime이 같으면 재사용.
	meta, metaChanged := a.loadMeta(), false
	presentRel := map[string]bool{}
	classify := func(e fs.DirEntry, full string) (string, string, int64) {
		var mtime int64
		if fi, err := e.Info(); err == nil {
			mtime = fi.ModTime().Unix()
		}
		rel, err := filepath.Rel(a.dataDir, full)
		if err != nil {
			rel = filepath.Base(full)
		}
		relKey := filepath.ToSlash(rel)
		presentRel[relKey] = true
		if e.IsDir() {
			return a.textKind(full), full, mtime // 폴더 분류는 저렴해서 캐시 불필요
		}
		if m, ok := meta[relKey]; ok && m.mtime == mtime {
			src := m.iconSrc
			if src == "" {
				src = full
			}
			return m.kind, src, mtime
		}
		kind, iconSrc := a.classifyAuto(full, false)
		src := iconSrc
		if src == full {
			src = ""
		}
		meta[relKey] = metaEnt{mtime: mtime, kind: kind, iconSrc: src}
		metaChanged = true
		return kind, iconSrc, mtime
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
			_, iconSrc, _ := classify(e, full)
			appendItem(e, full, e.IsDir(), ci, iconSrc)
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
			kind, iconSrc, _ := classify(e, full)
			appendItem(e, full, e.IsDir(), a.autoCol(kind), iconSrc)
		}
	}

	// 사라진 항목은 신규 기록/분류 캐시에서 정리
	for name := range seen {
		if !present[name] {
			delete(seen, name)
			seenChanged = true
		}
	}
	if seenChanged {
		a.saveSeen(seen)
	}
	for relKey := range meta {
		if !presentRel[relKey] {
			delete(meta, relKey)
			metaChanged = true
		}
	}
	if metaChanged {
		a.saveMeta(meta)
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

// ---- 분류 캐시(.meta): "상대경로 <TAB> mtime <TAB> 분류 <TAB> 아이콘소스" ----

type metaEnt struct {
	mtime   int64
	kind    string
	iconSrc string // 빈 값이면 파일 자신
}

func (a *app) metaPath() string {
	return filepath.Join(a.dataDir, ".meta")
}

func (a *app) loadMeta() map[string]metaEnt {
	m := map[string]metaEnt{}
	data, err := os.ReadFile(a.metaPath())
	if err != nil {
		return m
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		parts := strings.SplitN(line, "\t", 4)
		if len(parts) != 4 {
			continue
		}
		ts, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			continue
		}
		src := parts[3]
		if src == "-" {
			src = ""
		}
		m[parts[0]] = metaEnt{mtime: ts, kind: parts[2], iconSrc: src}
	}
	return m
}

func (a *app) saveMeta(m map[string]metaEnt) {
	var b strings.Builder
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		e := m[k]
		src := e.iconSrc
		if src == "" {
			src = "-"
		}
		fmt.Fprintf(&b, "%s\t%d\t%s\t%s\n", k, e.mtime, e.kind, src)
	}
	os.WriteFile(a.metaPath(), []byte(b.String()), 0o644)
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

// selfUpdate 는 새 exe를 내려받아 실행 중인 자신을 교체한다.
// 실행 중인 exe는 덮어쓸 수 없지만 이름 변경은 가능하다는 점을 이용한다:
// 현재 exe → gotool-old.exe 로 밀어내고, 받은 파일을 제자리에 넣는다.
func selfUpdate(url string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	dir := filepath.Dir(exe)
	tmp := filepath.Join(dir, "gotool-new.exe")
	oldp := filepath.Join(dir, "gotool-old.exe")

	client := &http.Client{Timeout: 3 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("다운로드 실패: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if len(data) < 100*1024 || data[0] != 'M' || data[1] != 'Z' {
		return fmt.Errorf("받은 파일이 올바른 실행 파일이 아닙니다 (%d바이트)", len(data))
	}
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return err
	}

	os.Remove(oldp) // 이전 잔여물 제거(실패 무시)
	if err := os.Rename(exe, oldp); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("실행 파일을 교체할 수 없습니다(폴더 권한 확인): %w", err)
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Rename(oldp, exe) // 롤백
		return err
	}
	return nil
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

// ---- 아이콘 캐시(.iconcache): RGBA 픽셀을 저장해 다음 실행부터 즉시 표시 ----

const iconCacheMagic = "GICO1"

func (a *app) iconCachePath() string {
	return filepath.Join(a.dataDir, ".iconcache")
}

func loadIconCache(path string) map[string]cacheEnt {
	m := map[string]cacheEnt{}
	data, err := os.ReadFile(path)
	if err != nil || len(data) < 5 || string(data[:5]) != iconCacheMagic {
		return m
	}
	r := bytes.NewReader(data[5:])
	for {
		var keyLen uint16
		if binary.Read(r, binary.LittleEndian, &keyLen) != nil {
			break
		}
		key := make([]byte, keyLen)
		if _, err := io.ReadFull(r, key); err != nil {
			break
		}
		var ent struct {
			Mtime  int64
			W, H   int32
			PixLen uint32
		}
		if binary.Read(r, binary.LittleEndian, &ent) != nil {
			break
		}
		if ent.PixLen > 16*1024*1024 || int64(ent.W)*int64(ent.H)*4 != int64(ent.PixLen) {
			break
		}
		pix := make([]byte, ent.PixLen)
		if _, err := io.ReadFull(r, pix); err != nil {
			break
		}
		m[string(key)] = cacheEnt{mtime: ent.Mtime, w: ent.W, h: ent.H, pix: pix}
	}
	return m
}

func saveIconCache(path string, m map[string]cacheEnt) {
	var b bytes.Buffer
	b.WriteString(iconCacheMagic)
	for key, ent := range m {
		binary.Write(&b, binary.LittleEndian, uint16(len(key)))
		b.WriteString(key)
		binary.Write(&b, binary.LittleEndian, struct {
			Mtime  int64
			W, H   int32
			PixLen uint32
		}{ent.mtime, ent.w, ent.h, uint32(len(ent.pix))})
		b.Write(ent.pix)
	}
	os.WriteFile(path, b.Bytes(), 0o644)
}

// hiconToRGBA 는 HICON을 RGBA 픽셀로 변환한다(캐시 저장용).
func hiconToRGBA(icon windows.Handle, w, h int32) []byte {
	hdc, _, _ := procGetDC.Call(0)
	if hdc == 0 {
		return nil
	}
	defer procReleaseDC.Call(0, hdc)
	memDC, _, _ := procCreateCompatibleDC.Call(hdc)
	if memDC == 0 {
		return nil
	}
	defer procDeleteDC.Call(memDC)

	bi := bitmapInfo{header: bitmapInfoHeader{
		biSize:     uint32(unsafe.Sizeof(bitmapInfoHeader{})),
		biWidth:    w,
		biHeight:   -h, // top-down
		biPlanes:   1,
		biBitCount: 32,
	}}
	var bits unsafe.Pointer
	hbmp, _, _ := procCreateDIBSection.Call(hdc, uintptr(unsafe.Pointer(&bi)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if hbmp == 0 || bits == nil {
		return nil
	}
	defer procDeleteObject.Call(hbmp)

	old, _, _ := procSelectObject.Call(memDC, hbmp)
	procDrawIconEx.Call(memDC, 0, 0, uintptr(icon), uintptr(w), uintptr(h), 0, 0, diNormal)
	procGdiFlush.Call()
	procSelectObject.Call(memDC, old)

	n := int(w * h * 4)
	src := unsafe.Slice((*byte)(bits), n)
	out := make([]byte, n)
	opaque := false
	for i := 0; i < n; i += 4 {
		out[i] = src[i+2] // R
		out[i+1] = src[i+1]
		out[i+2] = src[i] // B
		out[i+3] = src[i+3]
		if src[i+3] != 0 {
			opaque = true
		}
	}
	if !opaque {
		// 알파 채널이 없는 옛날 아이콘: 색이 있는 픽셀을 불투명 처리
		for i := 0; i < n; i += 4 {
			if out[i] != 0 || out[i+1] != 0 || out[i+2] != 0 {
				out[i+3] = 255
			}
		}
	}
	return out
}

// iconFromRGBA 는 캐시된 RGBA 픽셀로 HICON을 만든다.
func iconFromRGBA(w, h int32, pix []byte) windows.Handle {
	if int64(w)*int64(h)*4 != int64(len(pix)) || w <= 0 || h <= 0 {
		return 0
	}
	bi := bitmapInfo{header: bitmapInfoHeader{
		biSize:     uint32(unsafe.Sizeof(bitmapInfoHeader{})),
		biWidth:    w,
		biHeight:   -h,
		biPlanes:   1,
		biBitCount: 32,
	}}
	var bits unsafe.Pointer
	hbmColor, _, _ := procCreateDIBSection.Call(0, uintptr(unsafe.Pointer(&bi)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	if hbmColor == 0 || bits == nil {
		return 0
	}
	defer procDeleteObject.Call(hbmColor)

	n := len(pix)
	dst := unsafe.Slice((*byte)(bits), n)
	for i := 0; i < n; i += 4 {
		dst[i] = pix[i+2] // B
		dst[i+1] = pix[i+1]
		dst[i+2] = pix[i] // R
		dst[i+3] = pix[i+3]
	}

	hbmMask, _, _ := procCreateBitmap.Call(uintptr(w), uintptr(h), 1, 1, 0)
	if hbmMask == 0 {
		return 0
	}
	defer procDeleteObject.Call(hbmMask)

	ii := iconInfoStruct{fIcon: 1, hbmMask: windows.Handle(hbmMask), hbmColor: windows.Handle(hbmColor)}
	icon, _, _ := procCreateIconIndirect.Call(uintptr(unsafe.Pointer(&ii)))
	return windows.Handle(icon)
}

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

func createUIFont(dpi int32, weight int32) windows.Handle {
	height := -(9 * dpi) / 72 // 9pt
	face := utf16Ptr("맑은 고딕")
	f, _, _ := procCreateFontW.Call(
		uintptr(int(height)), 0, 0, 0,
		uintptr(weight),
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
