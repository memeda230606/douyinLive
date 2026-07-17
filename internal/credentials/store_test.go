package credentials

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type reversibleProtector struct{}

func (reversibleProtector) Protect(value []byte) ([]byte, error) {
	result := append([]byte("protected:"), value...)
	for left, right := 0, len(result)-1; left < right; left, right = left+1, right-1 {
		result[left], result[right] = result[right], result[left]
	}
	return result, nil
}

func (reversibleProtector) Unprotect(value []byte) ([]byte, error) {
	copyValue := append([]byte(nil), value...)
	for left, right := 0, len(copyValue)-1; left < right; left, right = left+1, right-1 {
		copyValue[left], copyValue[right] = copyValue[right], copyValue[left]
	}
	return bytes.TrimPrefix(copyValue, []byte("protected:")), nil
}

func TestFileStorePersistsProtectedCredentialsAndStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials.dat")
	now := func() time.Time { return time.UnixMilli(1_700_000_000_123) }
	store, err := openFileStore(path, reversibleProtector{}, now)
	if err != nil {
		t.Fatalf("openFileStore() error = %v", err)
	}
	secret := []byte("sessionid=private-cookie-value")
	status, err := store.Put(context.Background(), "room:test:cookie", secret)
	if err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if !status.Configured || status.UpdatedAt != now().UnixMilli() {
		t.Fatalf("unexpected status: %#v", status)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if bytes.Contains(data, secret) {
		t.Fatal("credential file contains plaintext secret")
	}

	reopened, err := openFileStore(path, reversibleProtector{}, now)
	if err != nil {
		t.Fatalf("reopen credential store: %v", err)
	}
	got, err := reopened.Get(context.Background(), "room:test:cookie")
	if err != nil || !bytes.Equal(got, secret) {
		t.Fatalf("Get() = (%q, %v), want original secret", got, err)
	}
	if err := reopened.Delete(context.Background(), "room:test:cookie"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	status, err = reopened.Status(context.Background(), "room:test:cookie")
	if err != nil || status.Configured {
		t.Fatalf("Status() after delete = (%#v, %v)", status, err)
	}
}
