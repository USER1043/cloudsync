package notify

import "fmt"

// State is how the tray icon looks: its colour and tooltip follow what the
// sync is doing.
type State int

const (
	StateOK        State = iota // in sync
	StateBusy                   // catching up or uploading
	StateAttention              // offline, slowed down, retrying
	StateError                  // failing or needs the owner
	StatePaused                 // paused by the owner
	StateStopped                // sync isn't running
)

// Snapshot is what the tray menu shows and acts on, taken each time the
// menu opens.
type Snapshot struct {
	Status  string // greyed first line, e.g. "CloudSync: in sync"
	Running bool   // the sync process is running
	Paused  bool   // ... and paused by the owner

	Folders []string // names of the synced folders, for "Open folder"

	StartAtLogin    bool
	CanChangeLogin  bool // false if the installed program can't be found
	CanOpenLogFile  bool
	StartSyncCannot string // if set, why "Start sync" can't work (shown greyed)
}

// Controls are what the menu items do. Each runs on its own goroutine, so it
// may take a while. A nil callback leaves its item out of the menu.
type Controls struct {
	Snapshot func() Snapshot

	SyncNow         func()
	Pause, Resume   func()
	StartSync       func()
	StopSync        func()
	OpenFolder      func(i int)
	OpenLog         func()
	SetStartAtLogin func(on bool)
	Exit            func()
}

// Menu item ids. Folder i is idFolder+i. They start above 0, which the
// system uses for "nothing chosen".
const (
	idStatus = iota + 1
	idSyncNow
	idPauseResume
	idStartStop
	idLog
	idLogin
	idExit
	idFolder = 100
)

type menuItem struct {
	id        int
	label     string
	disabled  bool
	checked   bool
	separator bool
}

var sep = menuItem{separator: true}

// buildMenu lists the menu for s.
func buildMenu(c Controls, s Snapshot) []menuItem {
	var m []menuItem
	m = append(m, menuItem{id: idStatus, label: s.Status, disabled: true}, sep)

	if c.SyncNow != nil {
		m = append(m, menuItem{id: idSyncNow, label: "Sync now", disabled: !s.Running})
	}
	if s.Paused {
		if c.Resume != nil {
			m = append(m, menuItem{id: idPauseResume, label: "Resume syncing"})
		}
	} else if c.Pause != nil {
		m = append(m, menuItem{id: idPauseResume, label: "Pause syncing", disabled: !s.Running})
	}
	switch {
	case s.Running && c.StopSync != nil:
		m = append(m, menuItem{id: idStartStop, label: "Stop sync"})
	case !s.Running && c.StartSync != nil:
		m = append(m, menuItem{id: idStartStop, label: "Start sync", disabled: s.StartSyncCannot != ""})
	}
	m = append(m, sep)

	if c.OpenFolder != nil {
		for i, name := range s.Folders {
			label := "Open folder"
			if len(s.Folders) > 1 {
				label = fmt.Sprintf("Open folder: %s", name)
			}
			m = append(m, menuItem{id: idFolder + i, label: label})
		}
	}
	if c.OpenLog != nil {
		m = append(m, menuItem{id: idLog, label: "Open log", disabled: !s.CanOpenLogFile})
	}
	if c.SetStartAtLogin != nil {
		m = append(m, menuItem{id: idLogin, label: "Start at login", checked: s.StartAtLogin, disabled: !s.CanChangeLogin})
	}
	m = append(m, sep)
	if c.Exit != nil {
		m = append(m, menuItem{id: idExit, label: "Exit"})
	}
	return tidy(m)
}

// tidy drops separators that open or close the menu or repeat.
func tidy(m []menuItem) []menuItem {
	var out []menuItem
	for _, it := range m {
		if it.separator && (len(out) == 0 || out[len(out)-1].separator) {
			continue
		}
		out = append(out, it)
	}
	if n := len(out); n > 0 && out[n-1].separator {
		out = out[:n-1]
	}
	return out
}

// choose runs what the item with this id does, and reports whether there
// was one. The callback runs on its own goroutine: the menu belongs to the
// window's message loop, which must not wait for a flush or a restart.
func choose(c Controls, s Snapshot, id int) bool {
	run := func(f func()) bool {
		if f == nil {
			return false
		}
		go f()
		return true
	}
	switch {
	case id == idSyncNow:
		return run(c.SyncNow)
	case id == idPauseResume:
		if s.Paused {
			return run(c.Resume)
		}
		return run(c.Pause)
	case id == idStartStop:
		if s.Running {
			return run(c.StopSync)
		}
		return run(c.StartSync)
	case id == idLog:
		return run(c.OpenLog)
	case id == idLogin:
		if c.SetStartAtLogin == nil {
			return false
		}
		on := !s.StartAtLogin
		return run(func() { c.SetStartAtLogin(on) })
	case id == idExit:
		return run(c.Exit)
	case id >= idFolder && id-idFolder < len(s.Folders):
		if c.OpenFolder == nil {
			return false
		}
		i := id - idFolder
		return run(func() { c.OpenFolder(i) })
	}
	return false
}
