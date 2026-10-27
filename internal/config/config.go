// Package config loads CloudSync settings and resolves each sync root's
// local path for the running OS.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/BurntSushi/toml"
)

const appName = "cloudsync"

// Root maps one logical folder to its local path on each OS and to a
// folder in Google Drive.
type Root struct {
	Name    string `toml:"name"`
	Remote  string `toml:"remote"` // slash-separated path under My Drive, e.g. "CloudSync/MyFiles"
	Windows string `toml:"windows"`
	Linux   string `toml:"linux"`
}

// Config is the contents of config.toml.
type Config struct {
	Roots  []Root   `toml:"root"`
	Ignore []string `toml:"ignore"` // extra ignore patterns, added to the built-in defaults

	path string // file the config came from; empty for built-in defaults
}

// Default is used when no config.toml exists.
func Default() Config {
	return Config{Roots: []Root{{
		Name:    "MyFiles",
		Remote:  "CloudSync/MyFiles",
		Windows: `C:\MyFiles`,
		Linux:   "~/My_Files",
	}}}
}

// Dir returns the per-user config directory: %AppData%\cloudsync on
// Windows and ~/.config/cloudsync on Linux.
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, appName), nil
}

// Load reads config.toml from dir, falling back to Default when the file
// does not exist.
func Load(dir string) (Config, error) {
	p := filepath.Join(dir, "config.toml")
	var c Config
	if _, err := toml.DecodeFile(p, &c); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Default(), nil
		}
		return Config{}, fmt.Errorf("reading %s: %w", p, err)
	}
	if err := c.validate(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", p, err)
	}
	c.path = p
	return c, nil
}

// Path returns the file the config was loaded from, or "" for defaults.
func (c Config) Path() string { return c.path }

func (c Config) validate() error {
	if len(c.Roots) == 0 {
		return errors.New("no [[root]] entries")
	}
	seen := make(map[string]bool)
	for _, r := range c.Roots {
		if r.Name == "" {
			return errors.New("every [[root]] needs a name")
		}
		if seen[r.Name] {
			return fmt.Errorf("root name %q is used twice", r.Name)
		}
		seen[r.Name] = true
		if strings.Contains(r.Remote, `\`) {
			return fmt.Errorf("root %q: remote path must use / separators", r.Name)
		}
		if len(r.RemoteSegments()) == 0 {
			return fmt.Errorf("root %q: remote must name a folder inside My Drive", r.Name)
		}
	}
	return nil
}

// RemoteSegments splits the remote path into folder names.
func (r Root) RemoteSegments() []string {
	var segs []string
	for _, s := range strings.Split(r.Remote, "/") {
		if s != "" {
			segs = append(segs, s)
		}
	}
	return segs
}

// LocalPath returns the root's folder on the running OS, with ~ expanded.
func (r Root) LocalPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return localPathFor(runtime.GOOS, home, r)
}

func localPathFor(goos, home string, r Root) (string, error) {
	var p string
	switch goos {
	case "windows":
		p = r.Windows
	case "linux":
		p = r.Linux
	default:
		return "", fmt.Errorf("unsupported OS %q", goos)
	}
	if p == "" {
		return "", fmt.Errorf("root %q has no %s path", r.Name, goos)
	}
	if p == "~" {
		p = home
	} else if strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		p = filepath.Join(home, p[2:])
	}
	return filepath.Clean(p), nil
}
