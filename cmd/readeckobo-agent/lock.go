package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// instanceLock is an advisory single-instance lock (flock) held for the
// lifetime of the agent process. The udev hook, a NickelMenu entry and the
// loop mode could otherwise run concurrent sync passes that fight over the
// index, ledger and the Kobo DB.
type instanceLock struct {
	f *os.File
}

// acquireInstanceLock takes an exclusive, non-blocking flock on path,
// creating the file if needed. It fails when another process holds the lock.
func acquireInstanceLock(path string) (*instanceLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, errors.New("lock file is held by another process")
		}
		return nil, fmt.Errorf("flock: %w", err)
	}
	return &instanceLock{f: f}, nil
}

func (l *instanceLock) release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}
