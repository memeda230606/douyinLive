//go:build p4accacceptance && !windows

package main

import "errors"

func captureP4AcceptanceWindow(string) (p4AcceptanceScreenshot, error) {
	return p4AcceptanceScreenshot{}, errors.New("P4 acceptance screenshot requires Windows")
}
