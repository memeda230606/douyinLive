//go:build !windows

package credentials

import "errors"

type platformProtector struct{}

func (platformProtector) Protect([]byte) ([]byte, error) {
	return nil, errors.New("credential protection is only available on Windows")
}

func (platformProtector) Unprotect([]byte) ([]byte, error) {
	return nil, errors.New("credential protection is only available on Windows")
}
