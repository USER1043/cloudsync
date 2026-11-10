package shutdown

import (
	"os"
	"testing"

	"github.com/godbus/dbus/v5"
)

// inhibitors lists logind's inhibitors held by this process.
func inhibitors(t *testing.T) (what, mode []string) {
	t.Helper()
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		t.Skipf("no system bus: %v", err)
	}
	defer conn.Close()
	var list []struct {
		What, Who, Why, Mode string
		UID, PID             uint32
	}
	if err := conn.Object(login1, login1Path).Call(manager+".ListInhibitors", 0).Store(&list); err != nil {
		t.Skipf("logind not available: %v", err)
	}
	for _, in := range list {
		if in.PID == uint32(os.Getpid()) && in.Who == "CloudSync" {
			what, mode = append(what, in.What), append(mode, in.Mode)
		}
	}
	return what, mode
}

func TestLogindInhibitor(t *testing.T) {
	if _, err := dbus.ConnectSystemBus(); err != nil {
		t.Skipf("no system bus: %v", err)
	}
	g, err := Start(Hooks{})
	if err != nil {
		t.Skipf("logind refused: %v", err)
	}
	what, mode := inhibitors(t)
	if len(what) != 1 || what[0] != "shutdown" || mode[0] != "delay" {
		t.Errorf("inhibitors = %v %v; want one shutdown delay (never block, never sleep)", what, mode)
	}
	g.Close()
	if what, _ := inhibitors(t); len(what) != 0 {
		t.Errorf("inhibitor still held after Close: %v", what)
	}
}
