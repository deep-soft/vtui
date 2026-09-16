//go:build windows

package vtui

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Alt+F9 in a classic Windows console window, done the way Far Manager does
// it (far/interf.cpp, ChangeVideoMode): WM_SYSCOMMAND SC_MAXIMIZE or
// SC_RESTORE to the console window, then the screen buffer sized to what the
// window shows.
//
// The buffer is the part that makes it work. conhost never makes a window
// larger than its screen buffer, and f4 keeps its buffer exactly the size of
// the viewport, so a bare SC_MAXIMIZE has no room to grow into. Far grows the
// buffer to GetLargestConsoleWindowSize; so does this, before the window is
// maximized, and then brings the buffer down to the viewport the maximized
// or restored window actually has, through the same crash-safe sequence the
// alternate screen uses (planFit, f4 #397).
//
// Only a console with a real window of its own qualifies. Under a
// pseudoconsole (Windows Terminal and the like) GetConsoleWindow answers with
// a stand-in window, and the caller falls back to the xterm sequence as
// before.

var (
	procGetLargestConsoleWindowSize = kernel32.NewProc("GetLargestConsoleWindowSize")
	procIsZoomedConsole             = user32.NewProc("IsZoomed")
	procSendMessageWConsole         = user32.NewProc("SendMessageW")
)

func toggleConsoleMaximizedOS() bool {
	if !classicConsoleWindow() {
		DebugLog("CONSOLE: toggle maximized: no classic console window, not handled")
		return false
	}
	hwnd, _, _ := procGetConsoleWindowAlt.Call()
	if hwnd == 0 {
		return false
	}

	// CONOUT$ is the active screen buffer, whichever one that is: the one
	// f4 was started in, the alternate screen's, or the Win32 renderer's.
	out, err := windows.CreateFile(windows.StringToUTF16Ptr("CONOUT$"),
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		DebugLog("CONSOLE: toggle maximized: opening the active screen buffer failed: %v", err)
		return false
	}
	defer windows.CloseHandle(out)
	handle := syscall.Handle(out)

	// The alternate screen fits its buffer to the viewport whenever the
	// size is polled; keep that from undoing the grow below mid-way.
	conhostAltMu.Lock()
	defer conhostAltMu.Unlock()

	var before consoleScreenBufferInfo
	if ok, _, _ := procGetConsoleScreenBufferInfo.Call(uintptr(handle), uintptr(unsafe.Pointer(&before))); ok == 0 {
		DebugLog("CONSOLE: toggle maximized: GetConsoleScreenBufferInfo failed")
		return false
	}
	zoomed, _, _ := procIsZoomedConsole.Call(hwnd)
	bw, bh := viewportSize(before)

	cmd := uintptr(scRestore)
	if zoomed == 0 {
		cmd = scMaximize
		largest, _, _ := procGetLargestConsoleWindowSize.Call(uintptr(handle))
		lw, lh := int16(uint16(largest)), int16(uint16(largest>>16))
		if lw > 0 && lh > 0 {
			grown := Coord{X: max(before.dwSize.X, lw), Y: max(before.dwSize.Y, lh)}
			if grown != before.dwSize {
				procSetConsoleScreenBufferSize.Call(uintptr(handle), coordArg(grown))
			}
		}
		DebugLog("CONSOLE: toggle maximized: maximizing, viewport %dx%d, buffer %dx%d, largest window %dx%d",
			bw, bh, before.dwSize.X, before.dwSize.Y, lw, lh)
	} else {
		DebugLog("CONSOLE: toggle maximized: restoring, viewport %dx%d, buffer %dx%d",
			bw, bh, before.dwSize.X, before.dwSize.Y)
	}

	procSendMessageWConsole.Call(hwnd, wmSysCommand, cmd, 0)

	var after consoleScreenBufferInfo
	if ok, _, _ := procGetConsoleScreenBufferInfo.Call(uintptr(handle), uintptr(unsafe.Pointer(&after))); ok == 0 {
		DebugLog("CONSOLE: toggle maximized: GetConsoleScreenBufferInfo after the command failed")
		return true
	}
	aw, ah := viewportSize(after)
	nowZoomed, _, _ := procIsZoomedConsole.Call(hwnd)
	DebugLog("CONSOLE: toggle maximized: after the command IsZoomed=%v, viewport %dx%d, buffer %dx%d",
		nowZoomed != 0, aw, ah, after.dwSize.X, after.dwSize.Y)
	fitConsoleBuffer(handle, aw, ah)
	return true
}
