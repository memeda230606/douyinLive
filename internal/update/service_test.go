package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestServiceChecksAndDownloadsSignedUpdate(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	installer := []byte(strings.Repeat("installer", 4096))
	digest := sha256.Sum256(installer)
	payload := testPayload()
	payload.Installer.Size = int64(len(installer))
	payload.Installer.SHA256 = hex.EncodeToString(digest[:])
	envelope, err := Sign(payload, "test", privateKey)
	if err != nil {
		t.Fatal(err)
	}
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/channels/stable.json":
			response.Header().Set("Content-Type", "application/json")
			_, _ = response.Write(envelope)
		case "/" + payload.Installer.ObjectKey:
			response.Header().Set("ETag", `"artifact-v1"`)
			http.ServeContent(response, request, "installer.exe", time.Unix(0, 0), strings.NewReader(string(installer)))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	service, err := NewService(Options{
		BaseURL: server.URL, Channel: "stable", CurrentVersion: "0.2.0",
		TrustedKeys: map[string]ed25519.PublicKey{"test": publicKey}, Root: t.TempDir(),
		HTTPClient: server.Client(), Now: func() time.Time { return time.Unix(100, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	status, err := service.Check(context.Background())
	if err != nil || status.State != StateAvailable || status.AvailableVersion != "0.2.1" {
		t.Fatalf("Check = (%+v, %v)", status, err)
	}
	status, err = service.Prepare(context.Background())
	if err != nil || status.State != StateReady || status.DownloadedBytes != int64(len(installer)) {
		t.Fatalf("Prepare = (%+v, %v)", status, err)
	}
	path, verified, err := service.PreparedInstaller()
	if err != nil || verified.Payload.Version != "0.2.1" {
		t.Fatalf("PreparedInstaller = (%q, %+v, %v)", path, verified, err)
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != string(installer) {
		t.Fatalf("installer = (%d bytes, %v)", len(content), err)
	}
}

func TestServiceResumesDownloadWithETag(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	installer := []byte(strings.Repeat("resume", 8192))
	digest := sha256.Sum256(installer)
	payload := testPayload()
	payload.Installer.Size = int64(len(installer))
	payload.Installer.SHA256 = hex.EncodeToString(digest[:])
	envelope, err := Sign(payload, "test", privateKey)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var ranges []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/channels/stable.json" {
			_, _ = response.Write(envelope)
			return
		}
		if request.URL.Path == "/"+payload.Installer.ObjectKey {
			mu.Lock()
			ranges = append(ranges, request.Header.Get("Range"))
			mu.Unlock()
			response.Header().Set("ETag", `"resume-v1"`)
			http.ServeContent(response, request, "installer.exe", time.Unix(0, 0), strings.NewReader(string(installer)))
			return
		}
		http.NotFound(response, request)
	}))
	defer server.Close()
	root := t.TempDir()
	service, err := NewService(Options{
		BaseURL: server.URL, CurrentVersion: "0.2.0", TrustedKeys: map[string]ed25519.PublicKey{"test": publicKey},
		Root: root, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "downloads", payload.Version)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	final := filepath.Join(directory, filepath.Base(payload.Installer.ObjectKey))
	half := len(installer) / 2
	if err := os.WriteFile(final+".part", installer[:half], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(final+".part.etag", []byte(`"resume-v1"`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ranges) != 1 || ranges[0] != "bytes="+strconv.Itoa(half)+"-" {
		t.Fatalf("ranges = %v", ranges)
	}
}

func TestServiceRejectsBadHashAndTransferBlock(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	payload := testPayload()
	envelope, err := Sign(payload, "test", privateKey)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/channels/stable.json":
			_, _ = response.Write(envelope)
		case "/" + payload.Installer.ObjectKey:
			response.Header().Set("ETag", `"bad"`)
			_, _ = response.Write([]byte(strings.Repeat("x", int(payload.Installer.Size))))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	blocked := true
	service, err := NewService(Options{
		BaseURL: server.URL, CurrentVersion: "0.2.0", TrustedKeys: map[string]ed25519.PublicKey{"test": publicKey},
		Root: t.TempDir(), HTTPClient: server.Client(),
		CanTransfer: func() (bool, string) {
			if blocked {
				return false, "recording"
			}
			return true, ""
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status, err := service.Prepare(context.Background()); err == nil || !status.InstallBlocked {
		t.Fatalf("blocked Prepare = (%+v, %v)", status, err)
	}
	blocked = false
	if status, err := service.Prepare(context.Background()); err == nil || status.ErrorCode != "UPDATE_HASH_MISMATCH" {
		t.Fatalf("bad hash Prepare = (%+v, %v)", status, err)
	}
}

func TestServiceTreatsCurrentVersionAsIdleAndRejectsRedirect(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	payload := testPayload()
	payload.Version = "0.2.0"
	payload.Installer.ObjectKey = "releases/v0.2.0/app-windows-amd64-installer.exe"
	payload.ReleaseManifest.ObjectKey = "releases/v0.2.0/release-manifest.json"
	envelope, err := Sign(payload, "test", privateKey)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_, _ = response.Write(envelope)
	}))
	defer server.Close()
	service, err := NewService(Options{
		BaseURL: server.URL, CurrentVersion: "0.2.0", TrustedKeys: map[string]ed25519.PublicKey{"test": publicKey},
		Root: t.TempDir(), HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	status, err := service.Check(context.Background())
	if err != nil || status.State != StateIdle {
		t.Fatalf("Check = (%+v, %v)", status, err)
	}
}
