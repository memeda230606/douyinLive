//go:build windows

package app

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	windowsApplicationLeaseHelperEnv  = "DOUYINLIVE_APPLICATION_LEASE_HELPER"
	windowsApplicationLeaseRootEnv    = "DOUYINLIVE_APPLICATION_LEASE_ROOT"
	windowsApplicationLeaseReadyToken = "ready"
)

func TestWindowsApplicationInstanceLeaseMutualExclusion(t *testing.T) {
	root := filepath.Join(t.TempDir(), "same-root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	first, err := acquireApplicationInstanceLease(root)
	if err != nil {
		t.Fatalf("acquire first lease: %v", err)
	}
	firstOwned := true
	t.Cleanup(func() {
		if firstOwned {
			_ = first.Close()
		}
	})

	second, err := acquireApplicationInstanceLease(root)
	if second != nil || !errors.Is(err, ErrApplicationInstanceActive) ||
		err.Error() != ErrApplicationInstanceActive.Error() {
		t.Fatalf("acquire duplicate lease = (%T, %v), want stable active sentinel", second, err)
	}

	otherRoot := filepath.Join(t.TempDir(), "other-root")
	if err := os.MkdirAll(otherRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	other, err := acquireApplicationInstanceLease(otherRoot)
	if err != nil {
		t.Fatalf("acquire different-root lease: %v", err)
	}
	if err := other.Close(); err != nil {
		t.Fatalf("close different-root lease: %v", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("close first lease: %v", err)
	}
	firstOwned = false
	if err := first.Close(); err != nil {
		t.Fatalf("idempotent close first lease: %v", err)
	}
	reacquired, err := acquireApplicationInstanceLease(root)
	if err != nil {
		t.Fatalf("reacquire released lease: %v", err)
	}
	if err := reacquired.Close(); err != nil {
		t.Fatalf("close reacquired lease: %v", err)
	}
}

func TestWindowsApplicationInstanceLeaseHolderHelper(t *testing.T) {
	if os.Getenv(windowsApplicationLeaseHelperEnv) != "1" {
		return
	}
	lease, err := acquireApplicationInstanceLease(os.Getenv(windowsApplicationLeaseRootEnv))
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if _, err := fmt.Fprintln(os.Stdout, windowsApplicationLeaseReadyToken); err != nil {
		t.Fatal(err)
	}
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

func TestWindowsApplicationInstanceLeaseRejectsAcrossProcessesAndReleasesOnCrash(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cross-process-root")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(
		ctx,
		os.Args[0],
		"-test.run=^TestWindowsApplicationInstanceLeaseHolderHelper$",
	)
	command.Env = append(
		os.Environ(),
		windowsApplicationLeaseHelperEnv+"=1",
		windowsApplicationLeaseRootEnv+"="+root,
	)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		_ = stdin.Close()
		if !waited {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	})
	ready, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(ready) != windowsApplicationLeaseReadyToken {
		t.Fatalf("lease holder handshake = (%q, %v)", ready, err)
	}
	duplicate, err := acquireApplicationInstanceLease(root)
	if duplicate != nil || !errors.Is(err, ErrApplicationInstanceActive) ||
		err.Error() != ErrApplicationInstanceActive.Error() {
		t.Fatalf("cross-process duplicate lease = (%T, %v)", duplicate, err)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	waitErr := command.Wait()
	waited = true
	if waitErr == nil {
		t.Fatal("crashed lease holder exited successfully")
	}

	reacquired, err := acquireApplicationInstanceLease(root)
	if err != nil {
		t.Fatalf("reacquire after holder crash: %v", err)
	}
	if err := reacquired.Close(); err != nil {
		t.Fatalf("close reacquired lease: %v", err)
	}
}

func TestWindowsApplicationInstanceLeaseNameIsStableAndPrivate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Private-Data-Root")
	child := filepath.Join(root, "child")
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatal(err)
	}
	name, err := applicationInstanceLeaseName(root)
	if err != nil {
		t.Fatalf("applicationInstanceLeaseName() error = %v", err)
	}
	equivalent, err := applicationInstanceLeaseName(filepath.Join(child, ".."))
	if err != nil {
		t.Fatalf("equivalent lease name error = %v", err)
	}
	caseEquivalent, err := applicationInstanceLeaseName(strings.ToUpper(root))
	if err != nil {
		t.Fatalf("case-equivalent lease name error = %v", err)
	}
	if name != equivalent || name != caseEquivalent {
		t.Fatalf("normalized names differ: %q %q %q", name, equivalent, caseEquivalent)
	}
	if !strings.HasPrefix(name, applicationInstanceLeaseNamePrefix) {
		t.Fatalf("lease name = %q, want Global product prefix", name)
	}
	suffix := strings.TrimPrefix(name, applicationInstanceLeaseNamePrefix)
	decoded, decodeErr := hex.DecodeString(suffix)
	if decodeErr != nil || len(decoded) != 32 {
		t.Fatalf("lease suffix is not SHA-256: len=%d err=%v", len(decoded), decodeErr)
	}
	lowerName := strings.ToLower(name)
	if strings.Contains(lowerName, strings.ToLower(root)) ||
		strings.Contains(lowerName, strings.ToLower(filepath.Base(root))) {
		t.Fatalf("lease name leaked data-root material: %q", name)
	}
	otherRoot := filepath.Join(t.TempDir(), "Private-Data-Root")
	if err := os.MkdirAll(otherRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	otherName, err := applicationInstanceLeaseName(otherRoot)
	if err != nil {
		t.Fatal(err)
	}
	if otherName == name {
		t.Fatalf("different roots shared lease name %q", name)
	}
}
