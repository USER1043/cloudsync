package trash

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// move follows the freedesktop.org Trash specification: the file goes to
// files/ in a trash folder, with a .trashinfo record of where it came from in
// info/. Files on the home file system use the home trash; files on another
// file system or Btrfs subvolume, which can't be renamed into it, use that
// file system's own trash folder.
func move(abs string) error {
	if _, err := os.Lstat(abs); err != nil {
		return err
	}
	home, err := homeTrash()
	if err != nil {
		return err
	}
	err = moveTo(home, abs)
	if !errors.Is(err, syscall.EXDEV) {
		return err
	}
	top, err := topTrash(abs)
	if err != nil {
		return err
	}
	return moveTo(top, abs)
}

// homeTrash returns $XDG_DATA_HOME/Trash, by default ~/.local/share/Trash.
func homeTrash() (string, error) {
	if d := os.Getenv("XDG_DATA_HOME"); filepath.IsAbs(d) {
		return filepath.Join(d, "Trash"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "Trash"), nil
}

// topTrash returns the trash folder for the file system holding abs:
// $topdir/.Trash/$uid if the administrator set up a shared .Trash (sticky
// and not a symlink, as the specification requires), else $topdir/.Trash-$uid.
func topTrash(abs string) (string, error) {
	top, err := mountTop(abs)
	if err != nil {
		return "", err
	}
	uid := strconv.Itoa(os.Getuid())
	if fi, err := os.Lstat(filepath.Join(top, ".Trash")); err == nil && fi.IsDir() && fi.Mode()&fs.ModeSticky != 0 {
		return filepath.Join(top, ".Trash", uid), nil
	}
	return filepath.Join(top, ".Trash-"+uid), nil
}

// mountTop returns the topmost folder above abs on the same device.
func mountTop(abs string) (string, error) {
	dev := func(p string) (uint64, error) {
		fi, err := os.Lstat(p)
		if err != nil {
			return 0, err
		}
		return uint64(fi.Sys().(*syscall.Stat_t).Dev), nil
	}
	want, err := dev(abs)
	if err != nil {
		return "", err
	}
	top := abs
	for {
		parent := filepath.Dir(top)
		if parent == top {
			return top, nil
		}
		d, err := dev(parent)
		if err != nil {
			return "", err
		}
		if d != want {
			return top, nil
		}
		top = parent
	}
}

// moveTo moves abs into the trash folder dir. The .trashinfo file is created
// first, exclusively, which reserves a free name in files/ even if another
// program trashes a file of the same name at the same moment.
func moveTo(dir, abs string) error {
	files, info := filepath.Join(dir, "files"), filepath.Join(dir, "info")
	for _, d := range []string{files, info} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("creating the trash folder: %w", err)
		}
	}
	record := fmt.Sprintf("[Trash Info]\nPath=%s\nDeletionDate=%s\n",
		(&url.URL{Path: abs}).EscapedPath(), time.Now().Format("2006-01-02T15:04:05"))

	base := filepath.Base(abs)
	for i := 1; ; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s.%d", base, i)
		}
		infoPath := filepath.Join(info, name+".trashinfo")
		f, err := os.OpenFile(infoPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("writing the trash record: %w", err)
		}
		_, err = f.WriteString(record)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			if _, serr := os.Lstat(filepath.Join(files, name)); serr == nil {
				os.Remove(infoPath) // a leftover without a record; don't overwrite it
				continue
			}
			err = os.Rename(abs, filepath.Join(files, name))
		}
		if err != nil {
			os.Remove(infoPath)
			return err
		}
		return nil
	}
}
