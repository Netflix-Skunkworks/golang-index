//go:build unix

package pgtest

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive advisory lock on path (creating it if needed),
// blocking until it is acquired, and returns a function that releases it. The OS
// drops the lock if this process exits, so a crashed holder can't stall others.
func lockFile(path string) (unlock func(), err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
