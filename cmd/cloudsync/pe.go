package main

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
)

const (
	subsystemGUI     = 2 // IMAGE_SUBSYSTEM_WINDOWS_GUI: no console window
	subsystemConsole = 3 // IMAGE_SUBSYSTEM_WINDOWS_CUI
)

// makeWindowless copies the Windows program src to dst, marked as a GUI
// program so Windows opens no console window for it. Only the PE header's
// Subsystem field changes (what Microsoft's `editbin /SUBSYSTEM:WINDOWS`
// does); the code is the same, so the copy behaves the same, minus the
// console.
func makeWindowless(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	off, err := subsystemOffset(data)
	if err != nil {
		return fmt.Errorf("%s: %w", src, err)
	}
	switch s := binary.LittleEndian.Uint16(data[off:]); s {
	case subsystemConsole:
		binary.LittleEndian.PutUint16(data[off:], subsystemGUI)
	case subsystemGUI:
	default:
		return fmt.Errorf("%s: unexpected PE subsystem %d", src, s)
	}
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// subsystemOffset finds the Subsystem field of a PE file: 68 bytes into
// the optional header, for PE32 and PE32+ alike.
func subsystemOffset(data []byte) (int, error) {
	f, err := pe.NewFile(bytes.NewReader(data))
	if err != nil {
		return 0, fmt.Errorf("not a Windows program: %w", err)
	}
	f.Close()
	if len(data) < 0x40 {
		return 0, errors.New("file too short")
	}
	peHeader := int(binary.LittleEndian.Uint32(data[0x3c:]))
	off := peHeader + 4 + 20 + 68 // "PE\0\0", COFF header, then the field
	if off+2 > len(data) || !bytes.Equal(data[peHeader:peHeader+4], []byte("PE\x00\x00")) {
		return 0, errors.New("malformed PE header")
	}
	return off, nil
}
