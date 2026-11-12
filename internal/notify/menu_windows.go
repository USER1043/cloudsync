package notify

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	procCreatePopupMenu          = user32.NewProc("CreatePopupMenu")
	procAppendMenuW              = user32.NewProc("AppendMenuW")
	procTrackPopupMenu           = user32.NewProc("TrackPopupMenu")
	procDestroyMenu              = user32.NewProc("DestroyMenu")
	procGetCursorPos             = user32.NewProc("GetCursorPos")
	procSetForegroundWindow      = user32.NewProc("SetForegroundWindow")
	procCreateIconFromResourceEx = user32.NewProc("CreateIconFromResourceEx")
	procLoadIconW                = user32.NewProc("LoadIconW")
	procGetSystemMetrics         = user32.NewProc("GetSystemMetrics")
)

const (
	mfString    = 0x0000
	mfGrayed    = 0x0001
	mfDisabled  = 0x0002
	mfChecked   = 0x0008
	mfSeparator = 0x0800

	tpmRightButton = 0x0002
	tpmReturnCmd   = 0x0100

	smCxSmIcon     = 49 // GetSystemMetrics: the small icon width, which tray icons use
	wmNull         = 0x0000
	idiApplication = 32512
)

// showMenu opens the tray menu at the mouse. It runs on the window's
// message-loop thread, inside the window procedure, and returns once the
// menu is closed; the chosen item runs on its own goroutine.
func (t *tray) showMenu() {
	t.cmu.Lock()
	c := t.controls
	t.cmu.Unlock()
	if c.Snapshot == nil {
		return
	}
	s := c.Snapshot()

	h, _, _ := procCreatePopupMenu.Call()
	if h == 0 {
		return
	}
	defer procDestroyMenu.Call(h)
	for _, it := range buildMenu(c, s) {
		if it.separator {
			procAppendMenuW.Call(h, mfSeparator, 0, 0)
			continue
		}
		flags := uintptr(mfString)
		if it.disabled {
			flags |= mfGrayed | mfDisabled
		}
		if it.checked {
			flags |= mfChecked
		}
		label, err := windows.UTF16PtrFromString(it.label)
		if err != nil {
			continue
		}
		procAppendMenuW.Call(h, flags, uintptr(it.id), uintptr(unsafe.Pointer(label)))
	}

	var pt struct{ x, y int32 }
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	// Without this the menu stays open when the user clicks elsewhere.
	procSetForegroundWindow.Call(t.hwnd)
	cmd, _, _ := procTrackPopupMenu.Call(h, tpmRightButton|tpmReturnCmd,
		uintptr(uint32(pt.x)), uintptr(uint32(pt.y)), 0, t.hwnd, 0)
	procPostMessageW.Call(t.hwnd, wmNull, 0, 0)
	if cmd != 0 {
		choose(c, s, int(cmd))
	}
}

// iconFor returns the icon for st, drawing it the first time. The caller
// holds t.mu.
func (t *tray) iconFor(st State) uintptr {
	if h := t.icons[st]; h != 0 {
		return h
	}
	// The size the taskbar shows tray icons at on this display.
	size, _, _ := procGetSystemMetrics.Call(smCxSmIcon)
	if size == 0 {
		size = 16
	}
	bits := iconBits(st, int(size))
	h, _, _ := procCreateIconFromResourceEx.Call(uintptr(unsafe.Pointer(&bits[0])), uintptr(len(bits)),
		1, 0x00030000, size, size, 0)
	if h == 0 {
		// The standard application icon, if the drawn one can't be made.
		h, _, _ = procLoadIconW.Call(0, idiApplication)
	}
	t.icons[st] = h
	return h
}
