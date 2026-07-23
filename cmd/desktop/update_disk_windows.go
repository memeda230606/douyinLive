//go:build windows

package main

import (
	"errors"
	"fmt"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func checkUpdateDiskSpace(installDir, dataRoot string, installerSize int64) error {
	if installerSize <= 0 {
		return errors.New("UPDATE_INSTALLER_SIZE_INVALID")
	}
	systemRequired := uint64(installerSize*2 + 512<<20)
	dataRequired := uint64(installerSize + 256<<20)
	if err := requireFreeBytes(installDir, systemRequired); err != nil {
		return fmt.Errorf("UPDATE_SYSTEM_DISK_FULL: %w", err)
	}
	if filepath.VolumeName(installDir) == filepath.VolumeName(dataRoot) {
		if dataRequired > systemRequired {
			systemRequired = dataRequired
		}
		return requireFreeBytes(installDir, systemRequired)
	}
	if err := requireFreeBytes(dataRoot, dataRequired); err != nil {
		return fmt.Errorf("UPDATE_DATA_DISK_FULL: %w", err)
	}
	return nil
}

func requireFreeBytes(path string, required uint64) error {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	var available uint64
	if err := windows.GetDiskFreeSpaceEx(pointer, &available, nil, nil); err != nil {
		return err
	}
	if available < required {
		return fmt.Errorf("available bytes below required threshold")
	}
	return nil
}
