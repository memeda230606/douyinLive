//go:build p3accacceptance && windows

package main

import "golang.org/x/sys/windows"

func replaceP3ACCAcceptanceFile(temporary, target string) error {
	temporaryPointer, err := windows.UTF16PtrFromString(temporary)
	if err != nil {
		return err
	}
	targetPointer, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(
		temporaryPointer, targetPointer,
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
	)
}
