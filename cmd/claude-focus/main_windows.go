// Command claude-focus brings the Claude desktop app's window to the front,
// launching the app first if it isn't open. It is meant to be bound to a key,
// such as the Copilot key via PowerToys Keyboard Manager.
//
// Launching claude-desktop.exe directly isn't enough when Claude is already
// running: the launcher hands off to the existing instance, and Windows'
// foreground lock stops that instance from taking focus, so it activates
// behind whatever window the user was in.
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32                  = windows.NewLazySystemDLL("user32.dll")
	procGetWindow           = user32.NewProc("GetWindow")
	procGetWindowTextLength = user32.NewProc("GetWindowTextLengthW")
	procIsIconic            = user32.NewProc("IsIconic")
	procShowWindow          = user32.NewProc("ShowWindow")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procKeybdEvent          = user32.NewProc("keybd_event")
)

const (
	gwOwner         = 4
	swRestore       = 9
	vkMenu          = 0x12
	keyeventfKeyUp  = 0x0002
	launchWaitLimit = 5 * time.Second
)

func main() {
	if hwnd := findClaudeWindow(); hwnd != 0 {
		activate(hwnd)
		return
	}

	// Not open (or hidden to the tray): start it via its app execution alias,
	// then focus the window once it appears.
	launcher := filepath.Join(os.Getenv("LOCALAPPDATA"), `Microsoft\WindowsApps\claude-desktop.exe`)
	if err := exec.Command(launcher).Start(); err != nil {
		os.Exit(1)
	}
	for deadline := time.Now().Add(launchWaitLimit); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if hwnd := findClaudeWindow(); hwnd != 0 {
			activate(hwnd)
			return
		}
	}
}

// findClaudeWindow returns the first visible, titled, unowned top-level window
// belonging to a claude.exe process. Claude Code (the CLI) is also claude.exe,
// but it never owns a top-level window (its console belongs to the terminal),
// so only the desktop app matches.
func findClaudeWindow() windows.HWND {
	var found windows.HWND
	cb := windows.NewCallback(func(hwnd windows.HWND, _ uintptr) uintptr {
		if !windows.IsWindowVisible(hwnd) {
			return 1
		}
		if owner, _, _ := procGetWindow.Call(uintptr(hwnd), gwOwner); owner != 0 {
			return 1
		}
		if n, _, _ := procGetWindowTextLength.Call(uintptr(hwnd)); n == 0 {
			return 1
		}
		var pid uint32
		if _, err := windows.GetWindowThreadProcessId(hwnd, &pid); err != nil {
			return 1
		}
		if !strings.EqualFold(processImageName(pid), "claude.exe") {
			return 1
		}
		found = hwnd
		return 0
	})
	// EnumWindows reports an error when the callback stops enumeration early.
	_ = windows.EnumWindows(cb, unsafe.Pointer(nil))
	return found
}

// processImageName returns the executable file name of pid, or "" if it
// can't be queried.
func processImageName(pid uint32) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)

	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return ""
	}
	return filepath.Base(windows.UTF16ToString(buf[:size]))
}

func activate(hwnd windows.HWND) {
	if iconic, _, _ := procIsIconic.Call(uintptr(hwnd)); iconic != 0 {
		procShowWindow.Call(uintptr(hwnd), swRestore)
	}
	// A synthetic Alt tap counts as this process's input, which releases the
	// foreground lock so SetForegroundWindow is honored.
	procKeybdEvent.Call(vkMenu, 0, 0, 0)
	procKeybdEvent.Call(vkMenu, 0, keyeventfKeyUp, 0)
	procSetForegroundWindow.Call(uintptr(hwnd))
}
