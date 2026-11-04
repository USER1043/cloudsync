package trash

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procSHFileOperationW = windows.NewLazySystemDLL("shell32.dll").NewProc("SHFileOperationW")

// shFileOpStruct is SHFILEOPSTRUCTW. On 64-bit Windows it uses natural
// alignment, which Go's layout matches.
type shFileOpStruct struct {
	hwnd                  uintptr
	wFunc                 uint32
	pFrom                 *uint16
	pTo                   *uint16
	fFlags                uint16
	fAnyOperationsAborted int32
	hNameMappings         uintptr
	lpszProgressTitle     *uint16
}

const (
	foDelete           = 0x0003
	fofSilent          = 0x0004
	fofNoConfirmation  = 0x0010
	fofAllowUndo       = 0x0040 // to the Recycle Bin
	fofNoErrorUI       = 0x0400
	fofWantNukeWarning = 0x4000 // ask before deleting outright a file the Recycle Bin can't take
)

// move sends abs to the Recycle Bin with the shell, as Explorer does. If the
// Recycle Bin can't take it (too large, or a drive without one), Windows asks
// the owner before deleting it outright, instead of doing so silently.
func move(abs string) error {
	// pFrom is a list of paths, each ending in NUL, ended by an extra NUL.
	from, err := windows.UTF16FromString(abs)
	if err != nil {
		return err
	}
	from = append(from, 0)
	op := shFileOpStruct{
		wFunc:  foDelete,
		pFrom:  &from[0],
		fFlags: fofAllowUndo | fofNoConfirmation | fofSilent | fofNoErrorUI | fofWantNukeWarning,
	}
	r, _, _ := procSHFileOperationW.Call(uintptr(unsafe.Pointer(&op)))
	if r != 0 {
		return fmt.Errorf("moving %s to the Recycle Bin failed (shell error 0x%x)", abs, r)
	}
	if op.fAnyOperationsAborted != 0 {
		return fmt.Errorf("moving %s to the Recycle Bin was cancelled", abs)
	}
	return nil
}
