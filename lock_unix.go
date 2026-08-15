//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"fmt"
	"os"
	"syscall"
)

type scanLock struct {
	file *os.File
}

func acquireScanLock(lockPath string) (*scanLock, error) {
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open scan lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("another air scan is already running")
	}
	return &scanLock{file: file}, nil
}

func (l *scanLock) Close() error {
	unlockErr := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}
