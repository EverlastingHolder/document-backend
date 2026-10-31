//go:build darwin || linux

package ocr

import (
	"errors"
	"fmt"
	"syscall"
)

func signalManagedGroup(groupID int, signal syscall.Signal) error {
	if err := syscall.Kill(-groupID, signal); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal managed process group: %w", err)
	}
	return nil
}
