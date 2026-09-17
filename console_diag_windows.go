//go:build windows

package vtui

import (
	"fmt"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Diagnostics for Alt+F9 in a classic console window (f4 #199). Nothing in
// this file changes what the console does; it only writes to the debug log.
//
// Two effects are reported that the existing log lines cannot explain:
//
//   - on inbox conhost, after a maximize and restore, the window's client
//     area is 17 px -- one SM_CXVSCROLL at 96 dpi -- wider than the 120
//     columns it shows, with no scroll bar drawn (seldom on OpenConsole);
//   - holding Alt+F9 for a while leaves a screen that does not match the
//     window after the key is released (OpenConsole; not in Windows
//     Terminal).
//
// conhost applies part of a size change later, on its window thread:
// SetConsoleWindowInfo posts CM_SET_WINDOW_SIZE, and scroll bar updates are
// posted as CM_UPDATE_SCROLL_BARS (microsoft/terminal, getset.cpp and
// windowproc.cpp). A snapshot taken right after the console calls can
// therefore miss the final state, so each toggle also takes one some time
// later.

var (
	procGetClientRectConsole      = user32.NewProc("GetClientRect")
	procGetWindowLongWConsole     = user32.NewProc("GetWindowLongW")
	procGetWindowPlacementConsole = user32.NewProc("GetWindowPlacement")
	procMonitorFromWindowConsole  = user32.NewProc("MonitorFromWindow")
	procGetMonitorInfoWConsole    = user32.NewProc("GetMonitorInfoW")
	procGetWindowThreadProcessID  = user32.NewProc("GetWindowThreadProcessId")
)

const (
	gwlStyleConsole     = -16 // GWL_STYLE
	consoleLateSnapshot = 500 * time.Millisecond
)

var consoleToggleSeq atomic.Int64

// logConsoleState writes one line with everything conhost and user32 report
// about the screen buffer behind handle and the console window hwnd.
func logConsoleState(tag string, handle syscall.Handle, hwnd uintptr) {
	var info consoleScreenBufferInfo
	csbiOK, _, csbiErr := procGetConsoleScreenBufferInfo.Call(uintptr(handle), uintptr(unsafe.Pointer(&info)))
	var wr, cr win32Rect
	wrOK, _, _ := procGetWindowRectConsole.Call(hwnd, uintptr(unsafe.Pointer(&wr)))
	crOK, _, _ := procGetClientRectConsole.Call(hwnd, uintptr(unsafe.Pointer(&cr)))
	style := int32(gwlStyleConsole)
	st, _, _ := procGetWindowLongWConsole.Call(hwnd, uintptr(style))
	zoomed, _, _ := procIsZoomedConsole.Call(hwnd)
	csbi := "failed: " + fmt.Sprint(csbiErr)
	if csbiOK != 0 {
		csbi = fmt.Sprintf("buffer %dx%d, srWindow L%d T%d R%d B%d (%dx%d), cursor %d,%d",
			info.dwSize.X, info.dwSize.Y,
			info.srWindow.Left, info.srWindow.Top, info.srWindow.Right, info.srWindow.Bottom,
			int(info.srWindow.Right-info.srWindow.Left)+1, int(info.srWindow.Bottom-info.srWindow.Top)+1,
			info.dwCursorPosition.X, info.dwCursorPosition.Y)
	}
	DebugLog("CONSOLE: state %s: %s; window %dx%d px (ok=%v), client %dx%d px (ok=%v), WS_VSCROLL=%v WS_HSCROLL=%v, IsZoomed=%v",
		tag, csbi,
		wr.right-wr.left, wr.bottom-wr.top, wrOK != 0,
		cr.right-cr.left, cr.bottom-cr.top, crOK != 0,
		uint32(st)&wsVScroll != 0, uint32(st)&wsHScroll != 0, zoomed != 0)
}

// logConsoleStateLater takes the same snapshot after conhost has had time to
// run what it posted to its window thread. It opens the active screen buffer
// itself: the caller's handle is closed by then.
func logConsoleStateLater(tag string) {
	go func() {
		time.Sleep(consoleLateSnapshot)
		hwnd, _, _ := procGetConsoleWindowAlt.Call()
		out, err := windows.CreateFile(windows.StringToUTF16Ptr("CONOUT$"),
			windows.GENERIC_READ|windows.GENERIC_WRITE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
			nil, windows.OPEN_EXISTING, 0, 0)
		if err != nil {
			DebugLog("CONSOLE: state %s: opening the active screen buffer failed: %v", tag, err)
			return
		}
		defer windows.CloseHandle(out)
		logConsoleState(tag, syscall.Handle(out), hwnd)
	}()
}

func fitOpLabel(op fitOp) string {
	switch op {
	case fitGrowBuffer:
		return "grow buffer"
	case fitMoveCursor:
		return "move cursor"
	case fitWindow:
		return "window"
	case fitSizeBuffer:
		return "size buffer"
	}
	return fmt.Sprintf("op%d", int(op))
}

// Under a pseudoconsole (Windows Terminal) the window Alt+F9 acts on is the
// terminal's, not the console's. These lines record which window that was,
// and where it was and what state it was in before the command and some time
// after it, next to the monitor's work area -- enough to tell a maximize from
// a plain resize of the window from where it stood.

// logPseudoConsoleLookup records what pseudoConsoleOwner looked at.
func logPseudoConsoleLookup(owner uintptr) {
	h, _, _ := procGetConsoleWindowAlt.Call()
	DebugLog("CONSOLE: pseudoconsole lookup: GetConsoleWindow %#x class %q, GW_OWNER %#x class %q pid %d",
		h, consoleWindowClass(h), owner, consoleWindowClass(owner), consoleWindowPID(owner))
}

func consoleWindowClass(hwnd uintptr) string {
	if hwnd == 0 {
		return ""
	}
	var buf [128]uint16
	n, _, _ := procGetClassNameWAlt.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:n])
}

func consoleWindowPID(hwnd uintptr) uint32 {
	if hwnd == 0 {
		return 0
	}
	var pid uint32
	procGetWindowThreadProcessID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	return pid
}

// win32WindowPlacement is WINDOWPLACEMENT.
type win32WindowPlacement struct {
	length           uint32
	flags            uint32
	showCmd          uint32
	ptMinPosition    win32Point
	ptMaxPosition    win32Point
	rcNormalPosition win32Rect
}

// win32MonitorInfo is MONITORINFO.
type win32MonitorInfo struct {
	cbSize    uint32
	rcMonitor win32Rect
	rcWork    win32Rect
	dwFlags   uint32
}

const monitorDefaultToNearest = 2 // MONITOR_DEFAULTTONEAREST

func formatWin32Rect(r win32Rect) string {
	return fmt.Sprintf("L%d T%d R%d B%d (%dx%d)", r.left, r.top, r.right, r.bottom, r.right-r.left, r.bottom-r.top)
}

// logTerminalWindowState writes one line about the terminal window hwnd:
// its rectangle, IsZoomed, the show command and normal position from its
// placement, and the monitor it is on with that monitor's work area.
func logTerminalWindowState(tag string, hwnd uintptr) {
	var wr win32Rect
	wrOK, _, _ := procGetWindowRectConsole.Call(hwnd, uintptr(unsafe.Pointer(&wr)))
	zoomed, _, _ := procIsZoomedConsole.Call(hwnd)
	wp := win32WindowPlacement{length: uint32(unsafe.Sizeof(win32WindowPlacement{}))}
	wpOK, _, _ := procGetWindowPlacementConsole.Call(hwnd, uintptr(unsafe.Pointer(&wp)))
	mi := win32MonitorInfo{cbSize: uint32(unsafe.Sizeof(win32MonitorInfo{}))}
	monitor, _, _ := procMonitorFromWindowConsole.Call(hwnd, monitorDefaultToNearest)
	miOK := uintptr(0)
	if monitor != 0 {
		miOK, _, _ = procGetMonitorInfoWConsole.Call(monitor, uintptr(unsafe.Pointer(&mi)))
	}
	DebugLog("CONSOLE: terminal window %s: hwnd %#x, window %s (ok=%v), IsZoomed=%v, placement showCmd=%d normal %s (ok=%v), monitor %s work area %s (ok=%v)",
		tag, hwnd, formatWin32Rect(wr), wrOK != 0, zoomed != 0,
		wp.showCmd, formatWin32Rect(wp.rcNormalPosition), wpOK != 0,
		formatWin32Rect(mi.rcMonitor), formatWin32Rect(mi.rcWork), miOK != 0)
}

// logTerminalWindowStateLater takes the same snapshot after the terminal has
// had time to act on the posted command and resize the pseudoconsole.
func logTerminalWindowStateLater(tag string, hwnd uintptr) {
	go func() {
		time.Sleep(consoleLateSnapshot)
		logTerminalWindowState(tag, hwnd)
	}()
}
