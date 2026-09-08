package main

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Logger writes leveled, UTC-timestamped lines to an optional log file under
// the data directory and to a secondary writer (stderr when run manually).
// The file is truncated once it exceeds maxLogBytes (1 MB), per the agent
// spec; rotation happens before a write that would cross the limit.
const maxLogBytes = 1 << 20 // 1 MiB

type Logger struct {
	mu   sync.Mutex
	file *os.File
	out  io.Writer // secondary sink (usually stderr); may be nil
	path string
	size int64
}

// newLogger opens (creating/truncating as needed) the log file at path.
// out receives a copy of every line (pass nil to disable; io.Discard in
// tests). A nil/empty path disables file logging.
func newLogger(path string, out io.Writer) (*Logger, error) {
	l := &Logger{out: out, path: path}
	if path == "" {
		return l, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err == nil && st.Size() >= maxLogBytes {
		// Oversized from a previous session: start fresh.
		if err := f.Truncate(0); err == nil {
			_, _ = f.Seek(0, io.SeekStart)
			st, _ = f.Stat()
		}
	}
	if st != nil {
		l.size = st.Size()
	}
	l.file = f
	return l, nil
}

func (l *Logger) Info(format string, args ...any)  { l.logf("INFO", format, args...) }
func (l *Logger) Warn(format string, args ...any)  { l.logf("WARN", format, args...) }
func (l *Logger) Error(format string, args ...any) { l.logf("ERROR", format, args...) }

func (l *Logger) logf(level, format string, args ...any) {
	line := time.Now().UTC().Format(time.RFC3339) + " [" + level + "] " + fmt.Sprintf(format, args...) + "\n"
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		if l.size+int64(len(line)) > maxLogBytes {
			// Truncate in place (spec: >1MB truncate) and restart the
			// offset bookkeeping.
			_ = l.file.Truncate(0)
			_, _ = l.file.Seek(0, io.SeekStart)
			l.size = 0
		}
		if n, err := l.file.WriteString(line); err == nil {
			l.size += int64(n)
		}
	}
	if l.out != nil {
		_, _ = io.WriteString(l.out, line)
	}
}

func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		err := l.file.Close()
		l.file = nil
		return err
	}
	return nil
}
