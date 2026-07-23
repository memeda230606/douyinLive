//go:build windows

package update

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

type protectedSigningKey struct {
	Version             int    `json:"version"`
	KeyID               string `json:"keyId"`
	PublicKey           string `json:"publicKey"`
	ProtectedPrivateKey string `json:"protectedPrivateKey"`
}

func CreateProtectedSigningKey(path, keyID string) (string, error) {
	if strings.TrimSpace(path) == "" || strings.TrimSpace(keyID) == "" || len(keyID) > 64 {
		return "", errors.New("UPDATE_SIGNING_KEY_ARGUMENT_INVALID")
	}
	if _, err := os.Lstat(path); err == nil {
		return "", errors.New("UPDATE_SIGNING_KEY_ALREADY_EXISTS")
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	protected, err := protectLocalMachine(privateKey)
	if err != nil {
		return "", err
	}
	record := protectedSigningKey{
		Version: 1, KeyID: keyID, PublicKey: hex.EncodeToString(publicKey),
		ProtectedPrivateKey: base64.StdEncoding.EncodeToString(protected),
	}
	content, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	_, writeErr := file.Write(content)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return record.PublicKey, nil
}

func LoadProtectedSigningKey(path string) (string, ed25519.PrivateKey, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", nil, err
	}
	var record protectedSigningKey
	if err := decodeStrict(content, &record); err != nil {
		return "", nil, err
	}
	if record.Version != 1 || strings.TrimSpace(record.KeyID) == "" {
		return "", nil, errors.New("UPDATE_SIGNING_KEY_FILE_INVALID")
	}
	publicKey, err := DecodePublicKey(record.PublicKey)
	if err != nil {
		return "", nil, err
	}
	protected, err := base64.StdEncoding.DecodeString(record.ProtectedPrivateKey)
	if err != nil || len(protected) == 0 || len(protected) > 4096 {
		return "", nil, errors.New("UPDATE_SIGNING_KEY_FILE_INVALID")
	}
	privateBytes, err := unprotectLocalMachine(protected)
	if err != nil || len(privateBytes) != ed25519.PrivateKeySize {
		return "", nil, errors.New("UPDATE_SIGNING_KEY_DECRYPT_FAILED")
	}
	privateKey := ed25519.PrivateKey(privateBytes)
	if !privateKey.Public().(ed25519.PublicKey).Equal(publicKey) {
		return "", nil, errors.New("UPDATE_SIGNING_KEY_MISMATCH")
	}
	return record.KeyID, privateKey, nil
}

func protectLocalMachine(value []byte) ([]byte, error) {
	return cryptData(value, true)
}

func unprotectLocalMachine(value []byte) ([]byte, error) {
	return cryptData(value, false)
}

func cryptData(value []byte, protect bool) ([]byte, error) {
	if len(value) == 0 {
		return nil, errors.New("empty DPAPI input")
	}
	input := windows.DataBlob{Size: uint32(len(value)), Data: &value[0]}
	var output windows.DataBlob
	var err error
	if protect {
		err = windows.CryptProtectData(
			&input, nil, nil, 0, nil,
			windows.CRYPTPROTECT_LOCAL_MACHINE|windows.CRYPTPROTECT_UI_FORBIDDEN,
			&output,
		)
	} else {
		err = windows.CryptUnprotectData(&input, nil, nil, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output)
	}
	if err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(output.Data))))
	if output.Size == 0 || output.Data == nil {
		return nil, errors.New("empty DPAPI output")
	}
	result := make([]byte, int(output.Size))
	copy(result, unsafe.Slice(output.Data, int(output.Size)))
	return result, nil
}
