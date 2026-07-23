//go:build !windows

package main

func checkUpdateDiskSpace(string, string, int64) error {
	return nil
}
