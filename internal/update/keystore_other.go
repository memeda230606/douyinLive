//go:build !windows

package update

import (
	"crypto/ed25519"
	"errors"
)

func CreateProtectedSigningKey(string, string) (string, error) {
	return "", errors.New("UPDATE_SIGNING_KEY_WINDOWS_REQUIRED")
}

func LoadProtectedSigningKey(string) (string, ed25519.PrivateKey, error) {
	return "", nil, errors.New("UPDATE_SIGNING_KEY_WINDOWS_REQUIRED")
}
