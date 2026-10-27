package main

import (
	"testing"
	"time"

	"github.com/USER1043/cloudsync/internal/engine"
)

func TestRenderBar(t *testing.T) {
	const mb = 1 << 20
	cases := []struct {
		name    string
		pr      engine.Progress
		elapsed time.Duration
		want    string
	}{
		{"start", engine.Progress{Total: 629, TotalBytes: 400 * mb}, 0,
			"[>                   ]   0%  0/629 files  0 B / 400.0 MB"},
		{"half", engine.Progress{Done: 300, Total: 629, Bytes: 200 * mb, TotalBytes: 400 * mb}, 100 * time.Second,
			"[==========>         ]  50%  300/629 files  200.0 MB / 400.0 MB  2.0 MB/s  ETA 1m40s"},
		{"done", engine.Progress{Done: 629, Total: 629, Bytes: 400 * mb, TotalBytes: 400 * mb}, 200 * time.Second,
			"[====================] 100%  629/629 files  400.0 MB / 400.0 MB  2.0 MB/s"},
		{"no bytes", engine.Progress{Done: 1, Total: 4}, time.Second,
			"[=====>              ]  25%  1/4 files  0 B / 0 B"},
	}
	for _, c := range cases {
		if got := renderBar(c.pr, c.elapsed, 200); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestRenderBarFitsWidth(t *testing.T) {
	pr := engine.Progress{Done: 3, Total: 9, Bytes: 5 << 20, TotalBytes: 9 << 20}
	cases := []struct {
		width int
		want  string
	}{
		{100, "[===========>        ]  55%  3/9 files  5.0 MB / 9.0 MB  512.0 KB/s  ETA 8s"},
		{70, "[========>      ]  55%  3/9 files  5.0 MB / 9.0 MB  512.0 KB/s  ETA 8s"}, // bar shrunk
		{60, "[=========>       ]  55%  3/9 files  5.0 MB / 9.0 MB  ETA 8s"},           // speed dropped
		{40, "[=======>      ]  55%  3/9 files  ETA 8s"},                               // sizes dropped
		{25, "[=====>    ]  55%  ETA 8s"},                                              // files dropped
		{15, " 55%  ETA 8s"},                                                           // no room for a bar
	}
	for _, c := range cases {
		got := renderBar(pr, 10*time.Second, c.width)
		if got != c.want {
			t.Errorf("width %d:\n got %q\nwant %q", c.width, got, c.want)
		}
		if len(got) > c.width {
			t.Errorf("width %d: line is %d long", c.width, len(got))
		}
	}
}
