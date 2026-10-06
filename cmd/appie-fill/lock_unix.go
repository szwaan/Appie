//go:build unix

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockInstance keeps a second appie-fill on this machine from opening an
// order while the first has one open: AH works on one order per account at
// a time. The kernel drops the lock when the process dies, so a crash never
// leaves it stale.
func lockInstance() (unlock func(), err error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "appie", "appie-fill.lock")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("appie-fill is already running (%s)", path)
		}
		return nil, err
	}
	return func() { f.Close() }, nil
}
