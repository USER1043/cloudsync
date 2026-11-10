package main

import (
	"strings"
	"testing"
)

func TestUnitFile(t *testing.T) {
	p := placement{name: "cloudsync-1234abcd", exe: "/home/u/state dir/bin/cloudsync",
		env: map[string]string{"CLOUDSYNC_STATE_DIR": "/home/u/state dir", "CLOUDSYNC_CONFIG": "/home/u/100%/config.toml"}}
	u := unitFile(p)
	for _, want := range []string{
		`ExecStart="/home/u/state dir/bin/cloudsync" run`,
		`Environment="CLOUDSYNC_CONFIG=/home/u/100%%/config.toml"`, // % doubled for systemd
		`Environment="CLOUDSYNC_STATE_DIR=/home/u/state dir"`,
		"RestartPreventExitStatus=3",
		"TimeoutStopSec=45",
		"PartOf=graphical-session.target",
		"WantedBy=graphical-session.target",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("unit file lacks %q:\n%s", want, u)
		}
	}
	if strings.Index(u, "CLOUDSYNC_CONFIG") > strings.Index(u, "CLOUDSYNC_STATE_DIR") {
		t.Error("environment not in a stable order")
	}
}
