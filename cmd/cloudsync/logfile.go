package main

import (
	"os"
	"path/filepath"
	"sync"
)

const (
	logFileName = "cloudsync.log"
	logMaxBytes = 5 << 20 // then it becomes cloudsync.log.1, replacing the older one
)

// logFile is `cloudsync run --log-file`'s output, for where there is no
// terminal or journal to read (the Windows background copy). It keeps the
// current log and one older one, at most about 10 MB together.
type logFile struct {
	path string

	mu   sync.Mutex
	f    *os.File
	size int64
}

func openLogFile(dir string) (*logFile, error) {
	l := &logFile{path: filepath.Join(dir, logFileName)}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := l.open(); err != nil {
		return nil, err
	}
	if l.size >= logMaxBytes {
		if err := l.rotate(); err != nil {
			return nil, err
		}
	}
	return l, nil
}

func (l *logFile) open() error {
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.size = f, st.Size()
	return nil
}

func (l *logFile) rotate() error {
	l.f.Close()
	if err := os.Rename(l.path, l.path+".1"); err != nil {
		return err
	}
	return l.open()
}

func (l *logFile) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.size+int64(len(p)) > logMaxBytes && l.size > 0 {
		if err := l.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

func (l *logFile) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}
