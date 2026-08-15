//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package main

import (
	"fmt"
	"os"
)

type scanLock struct {
	file *os.File
	path string
}

func acquireScanLock(lockPath string) (*scanLock, error) {
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("another air scan may already be running; remove %s if it is stale", lockPath)
		}
		return nil, fmt.Errorf("open scan lock: %w", err)
	}
	return &scanLock{file: file, path: lockPath}, nil
}

func (l *scanLock) Close() error {
	closeErr := l.file.Close()
	removeErr := os.Remove(l.path)
	if closeErr != nil {
		return closeErr
	}
	return removeErr
}
