package shutdown

import (
	"context"
	"fmt"
	"sync"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	login1     = "org.freedesktop.login1"
	login1Path = "/org/freedesktop/login1"
	manager    = "org.freedesktop.login1.Manager"
)

// startingTime is how long Hooks.Starting gets, within logind's default
// 5 s InhibitDelayMaxSec.
const startingTime = 4 * time.Second

// logind holds a delay inhibitor while no shutdown is under way, so
// PrepareForShutdown(true) waits for Hooks.Starting before going on.
type logind struct {
	h    Hooks
	conn *dbus.Conn
	sigs chan *dbus.Signal

	mu sync.Mutex
	fd int // the inhibitor's file descriptor; -1 if none is held
}

func start(h Hooks) (backend, error) {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, fmt.Errorf("connecting to the system bus: %w", err)
	}
	l := &logind{h: h, conn: conn, sigs: make(chan *dbus.Signal, 8), fd: -1}
	// Only logind's own signal counts, not a lookalike another program sends.
	if err := conn.AddMatchSignal(
		dbus.WithMatchSender(login1),
		dbus.WithMatchObjectPath(login1Path),
		dbus.WithMatchInterface(manager),
		dbus.WithMatchMember("PrepareForShutdown"),
	); err != nil {
		conn.Close()
		return nil, fmt.Errorf("listening for shutdowns: %w", err)
	}
	conn.Signal(l.sigs)
	if err := l.take(); err != nil {
		conn.Close()
		return nil, err
	}
	go l.loop()
	return l, nil
}

// take asks logind to wait for CloudSync at the next shutdown.
func (l *logind) take() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fd >= 0 {
		return nil
	}
	var fd dbus.UnixFD
	err := l.conn.Object(login1, login1Path).Call(manager+".Inhibit", 0,
		"shutdown", "CloudSync", "Saving the list of files not uploaded to Google Drive yet", "delay").Store(&fd)
	if err != nil {
		return fmt.Errorf("asking logind to wait for CloudSync at shutdown: %w", err)
	}
	l.fd = int(fd)
	return nil
}

// release lets a shutdown that is waiting for CloudSync go on.
func (l *logind) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fd >= 0 {
		syscall.Close(l.fd)
		l.fd = -1
	}
}

func (l *logind) loop() {
	for sig := range l.sigs {
		if sig.Name != manager+".PrepareForShutdown" || len(sig.Body) != 1 {
			continue
		}
		starting, _ := sig.Body[0].(bool)
		if !starting {
			// The shutdown was cancelled: be ready for the next one.
			_ = l.take()
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), startingTime)
		l.h.Starting(ctx)
		cancel()
		l.release()
	}
}

func (l *logind) recheck() {}

func (l *logind) close() {
	l.conn.RemoveSignal(l.sigs)
	l.release()
	l.conn.Close()
}
