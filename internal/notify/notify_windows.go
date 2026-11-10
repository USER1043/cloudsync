package notify

import (
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32                    = windows.NewLazySystemDLL("user32.dll")
	shell32                   = windows.NewLazySystemDLL("shell32.dll")
	procRegisterClassExW      = user32.NewProc("RegisterClassExW")
	procCreateWindowExW       = user32.NewProc("CreateWindowExW")
	procDefWindowProcW        = user32.NewProc("DefWindowProcW")
	procGetMessageW           = user32.NewProc("GetMessageW")
	procDispatchMessageW      = user32.NewProc("DispatchMessageW")
	procPostMessageW          = user32.NewProc("PostMessageW")
	procPostQuitMessage       = user32.NewProc("PostQuitMessage")
	procLoadIconW             = user32.NewProc("LoadIconW")
	procRegisterWindowMessage = user32.NewProc("RegisterWindowMessageW")
	procShellNotifyIconW      = shell32.NewProc("Shell_NotifyIconW")
)

const (
	wmDestroy = 0x0002
	wmClose   = 0x0010
	wmTray    = 0x8000 + 2 // WM_APP + 2: messages from the icon (unused)

	nimAdd        = 0
	nimModify     = 1
	nimDelete     = 2
	nimSetVersion = 4

	nifMessage = 0x01
	nifIcon    = 0x02
	nifTip     = 0x04
	nifInfo    = 0x10

	niifInfo    = 0x1
	niifWarning = 0x2

	notifyIconVersion4 = 4
	idiApplication     = 32512
)

type notifyIconData struct {
	size            uint32
	hwnd            uintptr
	id              uint32
	flags           uint32
	callbackMessage uint32
	icon            uintptr
	tip             [128]uint16
	state           uint32
	stateMask       uint32
	info            [256]uint16
	version         uint32 // also uTimeout
	infoTitle       [64]uint16
	infoFlags       uint32
	guidItem        windows.GUID
	balloonIcon     uintptr
}

type wndClassEx struct {
	size       uint32
	style      uint32
	wndProc    uintptr
	clsExtra   int32
	wndExtra   int32
	instance   windows.Handle
	icon       windows.Handle
	cursor     windows.Handle
	background windows.Handle
	menuName   *uint16
	className  *uint16
	iconSm     windows.Handle
}

type msg struct {
	hwnd    uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	pt      struct{ x, y int32 }
	private uint32
}

// tray is a CloudSync icon in the notification area. Its balloon messages
// are what Windows 10 and 11 show as notifications. It needs a window, a
// hidden one, to own the icon; if Explorer restarts, the icon is added
// again when the new taskbar announces itself.
type tray struct {
	hwnd           uintptr
	taskbarCreated uint32
	closed         sync.Once

	mu   sync.Mutex
	icon uintptr
}

func newBackend() (backend, error) {
	t := &tray{}
	ready := make(chan error, 1)
	go t.run(ready)
	if err := <-ready; err != nil {
		return nil, err
	}
	return t, nil
}

func (t *tray) run(ready chan<- error) {
	runtime.LockOSThread()
	var inst windows.Handle
	if err := windows.GetModuleHandleEx(0, nil, &inst); err != nil {
		ready <- fmt.Errorf("finding the program's module: %w", err)
		return
	}
	name, _ := windows.UTF16PtrFromString("CloudSyncTrayWindow")
	wc := wndClassEx{wndProc: windows.NewCallback(t.proc), instance: inst, className: name}
	wc.size = uint32(unsafe.Sizeof(wc))
	if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		ready <- fmt.Errorf("registering the tray window: %w", err)
		return
	}
	title, _ := windows.UTF16PtrFromString("CloudSync")
	hwnd, _, err := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(title)),
		0, 0, 0, 0, 0, 0, 0, uintptr(inst), 0)
	if hwnd == 0 {
		ready <- fmt.Errorf("creating the tray window: %w", err)
		return
	}
	t.hwnd = hwnd
	tc, _ := windows.UTF16PtrFromString("TaskbarCreated")
	r, _, _ := procRegisterWindowMessage.Call(uintptr(unsafe.Pointer(tc)))
	t.taskbarCreated = uint32(r)
	// The standard application icon until CloudSync has its own.
	t.icon, _, _ = procLoadIconW.Call(0, idiApplication)
	if err := t.add(); err != nil {
		ready <- err
		return
	}
	ready <- nil
	var m msg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			return
		}
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func (t *tray) proc(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
	switch {
	case message == t.taskbarCreated && t.taskbarCreated != 0:
		t.add()
		return 0
	case message == wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, uintptr(message), wParam, lParam)
	return r
}

func (t *tray) data() notifyIconData {
	d := notifyIconData{hwnd: t.hwnd, id: 1}
	d.size = uint32(unsafe.Sizeof(d))
	return d
}

// add puts the icon in the notification area.
func (t *tray) add() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	d := t.data()
	d.flags = nifMessage | nifIcon | nifTip
	d.callbackMessage = wmTray
	d.icon = t.icon
	copyUTF16(d.tip[:], "CloudSync")
	if r, _, err := procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&d))); r == 0 {
		return fmt.Errorf("adding the CloudSync icon to the taskbar: %w", err)
	}
	d.version = notifyIconVersion4
	procShellNotifyIconW.Call(nimSetVersion, uintptr(unsafe.Pointer(&d)))
	return nil
}

func (t *tray) show(m Message) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	d := t.data()
	d.flags = nifInfo
	copyUTF16(d.infoTitle[:], m.Title)
	copyUTF16(d.info[:], m.Body)
	d.infoFlags = niifInfo
	if m.Critical {
		d.infoFlags = niifWarning
	}
	if r, _, err := procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&d))); r == 0 {
		return fmt.Errorf("showing a notification: %w", err)
	}
	return nil
}

// clear takes down the balloon being shown, if any; Windows keeps no
// handle to a single notification, so key doesn't matter.
func (t *tray) clear(string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	d := t.data()
	d.flags = nifInfo
	procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&d)))
}

func (t *tray) close() {
	t.closed.Do(func() {
		t.mu.Lock()
		d := t.data()
		procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&d)))
		t.mu.Unlock()
		procPostMessageW.Call(t.hwnd, wmClose, 0, 0)
	})
}

// copyUTF16 copies s into a fixed-size, NUL-terminated buffer, cutting it
// short with "…" if it doesn't fit.
func copyUTF16(dst []uint16, s string) {
	u, err := windows.UTF16FromString(s)
	if err != nil {
		return
	}
	if len(u) > len(dst) {
		u = append(u[:len(dst)-2], '…', 0)
	}
	copy(dst, u)
}
