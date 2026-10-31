//go:build darwin || linux

package telemetry

import (
	"os"
	"syscall"
)

func lockLogFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}
func unlockLogFile(file *os.File) error { return syscall.Flock(int(file.Fd()), syscall.LOCK_UN) }
