//go:build !windows

package update

import (
	"errors"
	"io"
)

type PublishingCredentials struct {
	AccessKeyID     string `json:"accessKeyId"`
	AccessKeySecret string `json:"accessKeySecret"`
	SecurityToken   string `json:"securityToken,omitempty"`
}

func CreateProtectedPublishingCredentials(string, io.Reader) error {
	return errors.New("UPDATE_PUBLISH_CREDENTIAL_WINDOWS_REQUIRED")
}

func LoadProtectedPublishingCredentials(string) (PublishingCredentials, error) {
	return PublishingCredentials{}, errors.New("UPDATE_PUBLISH_CREDENTIAL_WINDOWS_REQUIRED")
}
