//go:build !windows

package update

import "errors"

const HealthMarkerSchema = "douyinlive-update-health/v1"

type HealthMarker struct {
	Schema  string `json:"schema"`
	Version string `json:"version"`
	Nonce   string `json:"nonce"`
}

func RunInstallHelper(string) error {
	return errors.New("UPDATE_HELPER_WINDOWS_REQUIRED")
}
