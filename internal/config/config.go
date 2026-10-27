// Package config loads CloudSync settings and resolves each sync root's
// local path.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

const (
	appName = "cloudsync"
	// FileName is the config file inside Dir.
	FileName = "config.toml"
)

// ErrNoConfig means CloudSync has not been set up on this computer.
var ErrNoConfig = errors.New("CloudSync is not set up on this computer yet; run `cloudsync init`")

// Root is one local folder and the Google Drive folder it syncs with.
type Root struct {
	Name   string `toml:"name"`   // key for this root in the state database; keep it unchanged
	Remote string `toml:"remote"` // slash-separated path under My Drive, e.g. "CloudSync/MyFiles"
	Path   string `toml:"path"`   // folder on this computer

	// Windows and Linux are the older way to give the path: one per OS, so a
	// single file could be shared. Path takes precedence when set.
	Windows string `toml:"windows"`
	Linux   string `toml:"linux"`
}

// Config is the contents of config.toml.
type Config struct {
	Ignore      []string `toml:"ignore"`        // extra gitignore-style rules, added to the built-in ones
	MaxFileSize string   `toml:"max_file_size"` // e.g. "500MB"; empty means no limit
	Roots       []Root   `toml:"root"`

	path     string // file the config came from
	maxBytes int64
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

// StateDir returns where the sync database lives: %LocalAppData%\cloudsync
// on Windows and $XDG_STATE_HOME/cloudsync (default ~/.local/state) on
// Linux. CLOUDSYNC_STATE_DIR overrides it.
func StateDir() (string, error) {
	if d := os.Getenv("CLOUDSYNC_STATE_DIR"); d != "" {
		return d, nil
	}
	if runtime.GOOS == "windows" {
		d := os.Getenv("LOCALAPPDATA")
		if d == "" {
			return "", errors.New("%LOCALAPPDATA% is not set")
		}
		return filepath.Join(d, appName), nil
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, appName), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", appName), nil
}

// FilePath returns the config file to use: CLOUDSYNC_CONFIG if set,
// otherwise config.toml in dir.
func FilePath(dir string) string {
	if p := os.Getenv("CLOUDSYNC_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(dir, FileName)
}

// Load reads the config file. It returns an error wrapping ErrNoConfig when
// there is none.
func Load(dir string) (Config, error) {
	p := FilePath(dir)
	var c Config
	if _, err := toml.DecodeFile(p, &c); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Config{}, fmt.Errorf("%w (no %s)", ErrNoConfig, p)
		}
		return Config{}, fmt.Errorf("reading %s: %w", p, err)
	}
	if err := c.validate(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", p, err)
	}
	c.path = p
	return c, nil
}

// Path returns the file the config was loaded from.
func (c Config) Path() string { return c.path }

// MaxBytes returns max_file_size in bytes, or 0 for no limit.
func (c Config) MaxBytes() int64 { return c.maxBytes }

func (c *Config) validate() error {
	n, err := ParseSize(c.MaxFileSize)
	if err != nil {
		return fmt.Errorf("max_file_size: %w", err)
	}
	c.maxBytes = n
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
		if r.Path == "" && r.Windows == "" && r.Linux == "" {
			return fmt.Errorf("root %q needs a path", r.Name)
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

// LocalPath returns the root's folder on this computer as an absolute path.
func (r Root) LocalPath() (string, error) {
	p := pathFor(runtime.GOOS, r)
	if p == "" {
		return "", fmt.Errorf("root %q has no path for this computer", r.Name)
	}
	return ExpandPath(p)
}

func pathFor(goos string, r Root) string {
	switch {
	case r.Path != "":
		return r.Path
	case goos == "windows":
		return r.Windows
	case goos == "linux":
		return r.Linux
	}
	return ""
}

// ExpandPath expands a leading ~ to the home folder and makes p absolute.
func ExpandPath(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") || strings.HasPrefix(p, `~\`) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, p[1:])
	}
	return filepath.Abs(p)
}

// ParseSize parses sizes like "500MB", "1.5 GB" or "2048" (bytes). Units
// are powers of 1024. An empty string means no limit and returns 0.
func ParseSize(s string) (int64, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" {
		return 0, nil
	}
	units := []struct {
		suffix string
		mult   float64
	}{{"TB", 1 << 40}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"B", 1}}
	mult := 1.0
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			s, mult = strings.TrimSpace(strings.TrimSuffix(s, u.suffix)), u.mult
			break
		}
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q is not a size like \"500MB\" or \"2GB\"", s)
	}
	return int64(n * mult), nil
}

// Write saves c as a commented config file in dir and returns its path.
func Write(dir string, c Config) (string, error) {
	var b strings.Builder
	b.WriteString(`# CloudSync configuration, written by ` + "`cloudsync init`" + `.
# Every computer keeps its own copy. On another computer, or the other OS of
# a dual-boot laptop, run ` + "`cloudsync init`" + ` there and pick the same Drive folder.

# Extra rules for files and folders to skip, written like .gitignore:
#   "*.iso"              skip a file type anywhere
#   "/College/Old/"      skip one folder (path from the synced folder)
#   "!keep-this.iso"     exception to an earlier rule
# A .cloudsyncignore file in any folder works the same way for that folder.
# Git repositories, node_modules, .venv and similar are always skipped.
ignore = [`)
	for i, p := range c.Ignore {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(tomlString(p))
	}
	b.WriteString("]\n\n# Skip files larger than this (KB, MB or GB).\n")
	if c.MaxFileSize != "" {
		fmt.Fprintf(&b, "max_file_size = %s\n", tomlString(c.MaxFileSize))
	} else {
		b.WriteString("# max_file_size = \"500MB\"\n")
	}
	for _, r := range c.Roots {
		fmt.Fprintf(&b, "\n[[root]]\nname   = %s  # key in CloudSync's sync records; do not change\nremote = %s  # folder in My Drive\npath   = %s  # folder on this computer\n",
			tomlString(r.Name), tomlString(r.Remote), tomlString(r.Path))
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	p := filepath.Join(dir, FileName)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return "", err
	}
	return p, nil
}

// tomlString quotes s for TOML, as a literal string ('...') when possible
// so Windows paths need no escaping.
func tomlString(s string) string {
	if !strings.ContainsAny(s, "'\n\r") {
		return "'" + s + "'"
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`).Replace(s) + `"`
}
