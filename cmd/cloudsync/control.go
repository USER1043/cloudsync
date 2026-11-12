package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gofrs/flock"

	"github.com/USER1043/cloudsync/internal/notify"
)

// ctlOp is something the tray (Windows) asks the running sync to do.
type ctlOp int

const (
	ctlSyncNow ctlOp = iota + 1 // run a pass now
	ctlPause                    // stop watching and uploading until resumed
	ctlResume
)

// statePaused is runStatus.State while the owner has paused syncing.
const statePaused = "paused by you"

const trayLockFile = "tray.lock"

// lockTray takes the lock that marks `cloudsync tray` as running, so a second
// one doesn't add a second icon. ok is false if another holds it.
func lockTray(dir string) (unlock func(), ok bool, err error) {
	lock := flock.New(filepath.Join(dir, trayLockFile))
	ok, err = lock.TryLock()
	if err != nil || !ok {
		return nil, false, err
	}
	return func() { lock.Unlock() }, true, nil
}

// trayIsRunning reports whether a `cloudsync tray` holds its lock.
func trayIsRunning(dir string) bool {
	unlock, ok, err := lockTray(dir)
	if ok {
		unlock()
		return false
	}
	return err == nil
}

// readStatus reads the status file `run` keeps; a missing or unreadable one
// reads as empty.
func readStatus(dir string) runStatus {
	var st runStatus
	if data, err := os.ReadFile(filepath.Join(dir, statusFile)); err == nil {
		json.Unmarshal(data, &st)
	}
	return st
}

// control passes op to the live loop. It never waits: a full queue means
// the loop is busy with a pass and the owner is clicking impatiently.
func (dm *daemon) control(op ctlOp) {
	select {
	case dm.ctl <- op:
	default:
	}
}

// trayState is the colour the tray icon should have for the status that
// `cloudsync run` last wrote, given whether it is still running.
func trayState(st runStatus, running bool) notify.State {
	if !running {
		if st.State == "stopped: needs you" {
			return notify.StateError
		}
		return notify.StateStopped
	}
	switch st.State {
	case "in sync":
		return notify.StateOK
	case statePaused:
		return notify.StatePaused
	case "failing":
		return notify.StateError
	case "offline", "slowed down by Google", "paused: Drive is full", "waiting for Google Drive", "retrying files that failed":
		return notify.StateAttention
	}
	// starting, catching up with Drive, uploading, stopping
	return notify.StateBusy
}

// statusLine is the tray's one-line status, also its tooltip.
func statusLine(st runStatus, running bool) string {
	var s string
	switch {
	case running && st.State != "":
		s = "CloudSync: " + st.State
	case running:
		s = "CloudSync: starting"
	case st.State == "stopped: needs you":
		s = "CloudSync: stopped, needs you"
	default:
		s = "CloudSync: not running"
	}
	if st.Detail != "" && (running || st.State == "stopped: needs you") {
		d := strings.Join(strings.Fields(st.Detail), " ")
		if r := []rune(d); len(r) > 70 {
			d = string(r[:69]) + "…"
		}
		s += " (" + d + ")"
	}
	return s
}

// warningDiff compares the warnings in the status file with those the tray
// has already shown (by key, with the time they were raised, which
// `warn` renews each time). It returns the ones to show now and the keys
// whose problem has gone, and updates shown.
func warningDiff(shown map[string]time.Time, now []warning) (fresh []warning, gone []string) {
	current := make(map[string]bool, len(now))
	// Oldest first, so several new ones appear in the order they happened.
	for i := len(now) - 1; i >= 0; i-- {
		w := now[i]
		current[w.Key] = true
		if at, ok := shown[w.Key]; !ok || !at.Equal(w.At) {
			shown[w.Key] = w.At
			fresh = append(fresh, w)
		}
	}
	for key := range shown {
		if !current[key] {
			delete(shown, key)
			gone = append(gone, key)
		}
	}
	return fresh, gone
}

// message turns a recorded warning back into a notification.
func (w warning) message() notify.Message {
	return notify.Message{Key: w.Key, Title: w.Title, Body: w.Body, Critical: w.Critical}
}

func (op ctlOp) String() string {
	switch op {
	case ctlSyncNow:
		return "sync now"
	case ctlPause:
		return "pause"
	case ctlResume:
		return "resume"
	}
	return fmt.Sprintf("ctlOp(%d)", int(op))
}
