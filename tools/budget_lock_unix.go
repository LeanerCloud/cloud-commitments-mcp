//go:build linux || darwin

package tools

import (
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// lockBudgetFile takes an exclusive flock on path (created 0600 if missing),
// retrying a non-blocking lock until timeout so a wedged peer cannot hang a
// purchase forever. The returned func releases the lock. The lock lives on a
// sibling file, not the state file, because the state file is replaced by
// rename on every write and a lock on the old inode would guard nothing.
func lockBudgetFile(path string, timeout time.Duration) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0o600) // #nosec G304 G703 -- operator-set path, not tool input
	if err != nil {
		return nil, fmt.Errorf("open budget lock file %s: %w", path, err)
	}
	deadline := time.Now().Add(timeout)
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) // #nosec G115 -- fd fits int
		if err == nil {
			return func() { _ = f.Close() }, nil // closing the fd drops the flock
		}
		if err != unix.EWOULDBLOCK && err != unix.EINTR {
			_ = f.Close()
			return nil, fmt.Errorf("lock budget file %s: %w", path, err)
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf("lock budget file %s: held by another process for over %s", path, timeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
