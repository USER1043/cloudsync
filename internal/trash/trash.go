// Package trash moves local files to the operating system's trash, where the
// owner can restore them: the freedesktop.org Trash on Linux (which KDE's and
// GNOME's file managers show), and the Recycle Bin on Windows. CloudSync
// never deletes a synced file outright.
package trash

import "path/filepath"

// Move moves the file or folder at path to the trash.
func Move(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	return move(abs)
}
