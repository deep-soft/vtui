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
	procGetClientRectConsole  = user32.NewProc("GetClientRect")
	procGetWindowLongWConsole = user32.NewProc("GetWindowLongW")
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
