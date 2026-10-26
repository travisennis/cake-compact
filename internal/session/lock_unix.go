//go:build !windows

package session

import (
	"fmt"
	"os"
	"syscall"
)

// lockShared takes a non-blocking shared advisory lock. Cake holds an
// exclusive advisory lock for the whole run it is appending; failing here is
// better than compacting a half-written snapshot.
func lockShared(file *os.File) error {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if err == nil {
		return nil
	}
	if err == syscall.ENOTSUP || err == syscall.EOPNOTSUPP || err == syscall.ENOSYS {
		return nil
	}
	return fmt.Errorf("session file is locked by another cake process; wait for it to finish: %w", err)
}
