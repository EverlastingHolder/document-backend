//go:build !darwin && !linux

package telemetry

import (
	"errors"
	"os"
)

func lockLogFile(*os.File) error   { return errors.New("logging file locking is unavailable") }
func unlockLogFile(*os.File) error { return nil }
