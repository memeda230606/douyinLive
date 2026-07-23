//go:build windows

package update

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type PublishingCredentials struct {
	AccessKeyID     string `json:"accessKeyId"`
	AccessKeySecret string `json:"accessKeySecret"`
	SecurityToken   string `json:"securityToken,omitempty"`
}

type protectedPublishingCredentials struct {
	Version   int    `json:"version"`
	Protected string `json:"protected"`
}

type publishingCredentialsPayload struct {
	Version int `json:"version"`
	PublishingCredentials
}

func CreateProtectedPublishingCredentials(path string, input io.Reader) error {
	if strings.TrimSpace(path) == "" || input == nil {
		return errors.New("UPDATE_PUBLISH_CREDENTIAL_ARGUMENT_INVALID")
	}
	if _, err := os.Lstat(path); err == nil {
		return errors.New("UPDATE_PUBLISH_CREDENTIAL_ALREADY_EXISTS")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	content, err := io.ReadAll(io.LimitReader(input, 16<<10))
	if err != nil || len(content) == 0 || len(content) >= 16<<10 {
		return errors.New("UPDATE_PUBLISH_CREDENTIAL_INPUT_INVALID")
	}
	var credentials PublishingCredentials
	if err := DecodeStrictJSON(content, &credentials); err != nil ||
		!validPublishingCredentials(credentials) {
		return errors.New("UPDATE_PUBLISH_CREDENTIAL_INPUT_INVALID")
	}
	payload, err := json.Marshal(publishingCredentialsPayload{
		Version: 1, PublishingCredentials: credentials,
	})
	if err != nil {
		return err
	}
	protected, err := protectLocalMachine(payload)
	if err != nil {
		return fmt.Errorf("UPDATE_PUBLISH_CREDENTIAL_PROTECT_FAILED: %w", err)
	}
	record, err := json.Marshal(protectedPublishingCredentials{
		Version: 1, Protected: base64.StdEncoding.EncodeToString(protected),
	})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(record)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

func LoadProtectedPublishingCredentials(path string) (PublishingCredentials, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return PublishingCredentials{}, err
	}
	var record protectedPublishingCredentials
	if err := DecodeStrictJSON(content, &record); err != nil ||
		record.Version != 1 {
		return PublishingCredentials{}, errors.New("UPDATE_PUBLISH_CREDENTIAL_FILE_INVALID")
	}
	protected, err := base64.StdEncoding.DecodeString(record.Protected)
	if err != nil || len(protected) == 0 || len(protected) > 16<<10 {
		return PublishingCredentials{}, errors.New("UPDATE_PUBLISH_CREDENTIAL_FILE_INVALID")
	}
	payloadBytes, err := unprotectLocalMachine(protected)
	if err != nil {
		return PublishingCredentials{}, errors.New("UPDATE_PUBLISH_CREDENTIAL_DECRYPT_FAILED")
	}
	var payload publishingCredentialsPayload
	if err := DecodeStrictJSON(payloadBytes, &payload); err != nil ||
		payload.Version != 1 || !validPublishingCredentials(payload.PublishingCredentials) {
		return PublishingCredentials{}, errors.New("UPDATE_PUBLISH_CREDENTIAL_FILE_INVALID")
	}
	return payload.PublishingCredentials, nil
}

func validPublishingCredentials(credentials PublishingCredentials) bool {
	if strings.TrimSpace(credentials.AccessKeyID) == "" ||
		strings.TrimSpace(credentials.AccessKeySecret) == "" ||
		len(credentials.AccessKeyID) > 128 || len(credentials.AccessKeySecret) > 256 ||
		len(credentials.SecurityToken) > 4096 {
		return false
	}
	for _, value := range []string{
		credentials.AccessKeyID, credentials.AccessKeySecret, credentials.SecurityToken,
	} {
		if strings.ContainsAny(value, "\r\n\x00") {
			return false
		}
	}
	return true
}
