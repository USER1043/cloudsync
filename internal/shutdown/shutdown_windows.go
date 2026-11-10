package shutdown

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32                         = windows.NewLazySystemDLL("user32.dll")
	procRegisterClassExW           = user32.NewProc("RegisterClassExW")
	procCreateWindowExW            = user32.NewProc("CreateWindowExW")
	procDefWindowProcW             = user32.NewProc("DefWindowProcW")
	procGetMessageW                = user32.NewProc("GetMessageW")
	procDispatchMessageW           = user32.NewProc("DispatchMessageW")
	procPostMessageW               = user32.NewProc("PostMessageW")
	procPostQuitMessage            = user32.NewProc("PostQuitMessage")
	procShutdownBlockReasonCreate  = user32.NewProc("ShutdownBlockReasonCreate")
	procShutdownBlockReasonDestroy = user32.NewProc("ShutdownBlockReasonDestroy")
)

const (
	wmDestroy         = 0x0002
	wmClose           = 0x0010
	wmQueryEndSession = 0x0011
	wmEndSession      = 0x0016
	wmRecheck         = 0x8000 + 1 // WM_APP + 1
)

// startingTime is how long Hooks.Starting gets. Windows waits for the
// owner while a shutdown is held, so this only bounds the work.
const startingTime = 15 * time.Second

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

// window is a hidden top-level window: Windows sends WM_QUERYENDSESSION
// only to top-level windows (not to message-only ones), and a shutdown
// block reason must belong to a window. Its messages are handled on one
// locked OS thread, which ShutdownBlockReasonCreate requires.
type window struct {
	h      Hooks
	hwnd   uintptr
	closed sync.Once

	// Used only on the window's thread.
	started bool // Starting has run for this shutdown
	held    bool // a block reason is set
}

func start(h Hooks) (backend, error) {
	w := &window{h: h}
	ready := make(chan error, 1)
	go w.run(ready)
	if err := <-ready; err != nil {
		return nil, err
	}
	return w, nil
}

func (w *window) run(ready chan<- error) {
	runtime.LockOSThread()
	var inst windows.Handle
	if err := windows.GetModuleHandleEx(0, nil, &inst); err != nil {
		ready <- fmt.Errorf("finding the program's module: %w", err)
		return
	}
	name, _ := windows.UTF16PtrFromString("CloudSyncShutdownWindow")
	wc := wndClassEx{wndProc: windows.NewCallback(w.proc), instance: inst, className: name}
	wc.size = uint32(unsafe.Sizeof(wc))
	if r, _, err := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		ready <- fmt.Errorf("registering the shutdown window: %w", err)
		return
	}
	title, _ := windows.UTF16PtrFromString("CloudSync")
	// Style 0 and no WS_VISIBLE: the window is never shown.
	hwnd, _, err := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(title)),
		0, 0, 0, 0, 0, 0, 0, uintptr(inst), 0)
	if hwnd == 0 {
		ready <- fmt.Errorf("creating the shutdown window: %w", err)
		return
	}
	w.hwnd = hwnd
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

func (w *window) proc(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case wmQueryEndSession:
		if !w.started {
			w.started = true
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), startingTime)
				defer cancel()
				w.h.Starting(ctx)
			}()
		}
		if w.hold(w.h.Holding()) {
			return 0 // not yet
		}
		return 1
	case wmEndSession:
		if wParam != 0 {
			w.h.Ending()
		} else {
			// The shutdown was cancelled.
			w.started = false
			w.hold("")
		}
		return 0
	case wmRecheck:
		if w.held {
			w.hold(w.h.Holding())
		}
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, uintptr(message), wParam, lParam)
	return r
}

// hold sets the reason Windows shows for holding the shutdown, or clears it
// when reason is "". It reports whether the shutdown is held.
func (w *window) hold(reason string) bool {
	if reason == "" {
		if w.held {
			procShutdownBlockReasonDestroy.Call(w.hwnd)
			w.held = false
		}
		return false
	}
	p, err := windows.UTF16PtrFromString(reason)
	if err != nil {
		return false
	}
	if r, _, _ := procShutdownBlockReasonCreate.Call(w.hwnd, uintptr(unsafe.Pointer(p))); r == 0 {
		return false
	}
	w.held = true
	return true
}

func (w *window) recheck() { procPostMessageW.Call(w.hwnd, wmRecheck, 0, 0) }

// close ends the window through WM_CLOSE, whose default handling destroys
// it; WM_DESTROY then ends the message loop.
func (w *window) close() {
	w.closed.Do(func() { procPostMessageW.Call(w.hwnd, wmClose, 0, 0) })
}
