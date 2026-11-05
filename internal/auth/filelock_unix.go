//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package auth

import (
	"os"

	"golang.org/x/sys/unix"
)

func tryLockAuthFile(file *os.File) (bool, error) {
	err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == unix.EWOULDBLOCK || err == unix.EINTR {
		return false, nil
	}
	return err == nil, err
}

func unlockAuthFile(file *os.File) {
	_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
