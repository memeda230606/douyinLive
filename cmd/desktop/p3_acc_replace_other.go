//go:build p3accacceptance && !windows

package main

import "os"

func replaceP3ACCAcceptanceFile(temporary, target string) error {
	return os.Rename(temporary, target)
}
