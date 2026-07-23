package update

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteStartupHealthMarkerIsOptInAndPathBound(t *testing.T) {
	root, err := filepath.Abs(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteStartupHealthMarker(root, "0.2.1"); err != nil {
		t.Fatalf("no environment error = %v", err)
	}

	nonce := "0123456789abcdef0123456789abcdef"
	path := filepath.Join(root, "updates", "health", nonce+".json")
	t.Setenv(healthFileEnvironment, path)
	t.Setenv(healthNonceEnvironment, nonce)
	t.Setenv(healthVersionEnvironment, "0.2.1")
	if err := WriteStartupHealthMarker(root, "0.2.1"); err != nil {
		t.Fatalf("WriteStartupHealthMarker() error = %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var marker HealthMarker
	if err := DecodeStrictJSON(content, &marker); err != nil {
		t.Fatal(err)
	}
	if marker.Schema != HealthMarkerSchema || marker.Version != "0.2.1" || marker.Nonce != nonce {
		t.Fatalf("marker = %+v", marker)
	}

	t.Setenv(healthFileEnvironment, filepath.Join(root, "outside.json"))
	if err := WriteStartupHealthMarker(root, "0.2.1"); errorCode(err) != "UPDATE_HEALTH_PATH_INVALID" {
		t.Fatalf("outside path error = %v", err)
	}
	t.Setenv(healthFileEnvironment, path)
	t.Setenv(healthVersionEnvironment, "0.2.2")
	if err := WriteStartupHealthMarker(root, "0.2.1"); errorCode(err) != "UPDATE_HEALTH_ENVIRONMENT_INVALID" {
		t.Fatalf("wrong version error = %v", err)
	}
}
