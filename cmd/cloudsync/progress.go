package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/USER1043/cloudsync/internal/engine"
)

// progressBar keeps a one-line bar at the bottom of a terminal while log
// lines scroll above it. It uses only carriage returns and spaces, so it
// works in every Windows and Linux terminal.
//
//	[=========>          ]  45%  283/629 files  170.1 MB / 377.5 MB  1.4 MB/s  ETA 2m28s
type progressBar struct {
	out   *os.File
	start time.Time
	stop  chan struct{}

	mu      sync.Mutex
	pr      engine.Progress
	shown   bool // a bar line is on screen
	lastLen int  // length of the bar line on screen
}

// newProgressBar returns a bar for out, or nil if out is not a terminal.
func newProgressBar(out *os.File) *progressBar {
	if !term.IsTerminal(int(out.Fd())) {
		return nil
	}
	return &progressBar{out: out, start: time.Now(), stop: make(chan struct{})}
}

// Write prints log output above the bar.
func (b *progressBar) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.clear()
	n, err := b.out.Write(p)
	b.draw()
	return n, err
}

// Update shows new progress. The first call also starts a ticker that keeps
// the speed and ETA fresh while a large file is transferring.
func (b *progressBar) Update(pr engine.Progress) {
	b.mu.Lock()
	defer b.mu.Unlock()
	first := !b.shown && pr.Done == 0
	b.pr, b.shown = pr, true
	b.draw()
	if first {
		go b.tick()
	}
}

func (b *progressBar) tick() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-b.stop:
			return
		case <-t.C:
			b.mu.Lock()
			b.draw()
			b.mu.Unlock()
		}
	}
}

// Finish stops the ticker and leaves the final bar on its own line.
func (b *progressBar) Finish() {
	close(b.stop)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.shown {
		b.draw()
		fmt.Fprintln(b.out)
		b.shown, b.lastLen = false, 0
	}
}

func (b *progressBar) clear() {
	if b.lastLen > 0 {
		fmt.Fprint(b.out, "\r"+strings.Repeat(" ", b.lastLen)+"\r")
		b.lastLen = 0
	}
}

func (b *progressBar) draw() {
	if !b.shown {
		return
	}
	width := 80
	if w, _, err := term.GetSize(int(b.out.Fd())); err == nil && w > 0 {
		width = w
	}
	line := renderBar(b.pr, time.Since(b.start), width-1) // one short of the edge, so it never wraps
	pad := ""
	if n := b.lastLen - len(line); n > 0 {
		pad = strings.Repeat(" ", n)
	}
	fmt.Fprint(b.out, "\r"+line+pad)
	b.lastLen = len(line)
}

const (
	maxBarWidth = 20
	minBarWidth = 10
)

// renderBar formats progress as a single line at most maxLen bytes long.
// On a narrow terminal it first shrinks the bar, then drops the speed, the
// byte counts and the file counts, in that order. The percentage and ETA
// stay; the bar itself goes last.
func renderBar(pr engine.Progress, elapsed time.Duration, maxLen int) string {
	frac := 1.0
	switch {
	case pr.TotalBytes > 0:
		frac = float64(pr.Bytes) / float64(pr.TotalBytes)
	case pr.Total > 0:
		frac = float64(pr.Done) / float64(pr.Total)
	}
	frac = min(max(frac, 0), 1)

	pct := fmt.Sprintf("%3d%%", int(frac*100))
	files := fmt.Sprintf("%d/%d files", pr.Done, pr.Total)
	size := fmt.Sprintf("%s / %s", humanBytes(pr.Bytes), humanBytes(pr.TotalBytes))
	var speed, eta string
	if secs := elapsed.Seconds(); secs >= 1 && pr.Bytes > 0 {
		rate := float64(pr.Bytes) / secs
		speed = humanBytes(int64(rate)) + "/s"
		if pr.Done < pr.Total {
			left := time.Duration(float64(pr.TotalBytes-pr.Bytes) / rate * float64(time.Second))
			eta = "ETA " + left.Round(time.Second).String()
		}
	}

	for _, parts := range [][]string{
		{pct, files, size, speed, eta},
		{pct, files, size, eta},
		{pct, files, eta},
		{pct, eta},
	} {
		stats := joinNonEmpty(parts)
		if width := maxLen - len(stats) - 3; width >= minBarWidth { // 3 = "[", "]" and a space
			return drawBar(frac, min(width, maxBarWidth)) + " " + stats
		}
	}
	line := joinNonEmpty([]string{pct, eta})
	if maxLen > 0 && len(line) > maxLen {
		line = line[:maxLen]
	}
	return line
}

// drawBar draws "[=====>    ]" with width columns between the brackets.
func drawBar(frac float64, width int) string {
	filled := int(frac * float64(width))
	bar := strings.Repeat("=", filled)
	if filled < width {
		bar += ">" + strings.Repeat(" ", width-filled-1)
	}
	return "[" + bar + "]"
}

func joinNonEmpty(parts []string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "  ")
}
