package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"

	"github.com/USER1043/cloudsync/internal/auth"
	"github.com/USER1043/cloudsync/internal/config"
	"github.com/USER1043/cloudsync/internal/gdrive"
)

// env is what every command needs: the config directory and the config.
type env struct {
	dir string
	cfg config.Config
}

func loadEnv() (*env, error) {
	dir, err := config.Dir()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(dir)
	if err != nil {
		return nil, err
	}
	return &env{dir: dir, cfg: cfg}, nil
}

// drive returns a Drive client using the stored token. It fails with
// auth.ErrNoClientConfig or auth.ErrNotLoggedIn until sign-in is set up.
func (e *env) drive(ctx context.Context) (*gdrive.Client, error) {
	oc, err := auth.LoadConfig(e.dir)
	if err != nil {
		return nil, err
	}
	hc, err := auth.Client(ctx, oc, auth.KeyringStore{})
	if err != nil {
		return nil, err
	}
	return gdrive.New(ctx, hc)
}

func runLogin(ctx context.Context) error {
	e, err := loadEnv()
	if err != nil {
		return err
	}
	oc, err := auth.LoadConfig(e.dir)
	if err != nil {
		return err
	}
	tok, err := auth.Login(ctx, oc, openBrowser, os.Stdout)
	if err != nil {
		return err
	}
	if err := (auth.KeyringStore{}).Save(tok); err != nil {
		return err
	}
	fmt.Println("Signed in. The token is saved in the system credential store.")
	return runWhoami(ctx)
}

func runLogout(ctx context.Context) error {
	if err := auth.Logout(ctx, auth.KeyringStore{}); err != nil {
		return err
	}
	fmt.Println("Signed out.")
	return nil
}

func runWhoami(ctx context.Context) error {
	e, err := loadEnv()
	if err != nil {
		return err
	}
	d, err := e.drive(ctx)
	if err != nil {
		return err
	}
	a, err := d.Account(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("Signed in as %s <%s>\n", a.Name, a.Email)
	if a.Limit > 0 {
		fmt.Printf("Drive storage: %s used of %s (%s free)\n",
			humanBytes(a.Usage), humanBytes(a.Limit), humanBytes(a.Limit-a.Usage))
	} else {
		fmt.Printf("Drive storage: %s used (no limit)\n", humanBytes(a.Usage))
	}
	return nil
}

func openBrowser(url string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
