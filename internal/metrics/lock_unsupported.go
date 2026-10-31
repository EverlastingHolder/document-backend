//go:build !darwin && !linux

package metrics

import (
	"errors"
	"os"
)

func lockState(*os.File) error   { return errors.New("metrics state locking is unavailable") }
func unlockState(*os.File) error { return nil }
