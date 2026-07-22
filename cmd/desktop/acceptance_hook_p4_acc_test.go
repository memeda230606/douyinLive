//go:build p4accacceptance

package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestP4AcceptanceClientResultValidation(t *testing.T) {
	checks := make(map[string]bool, len(p4AcceptanceChecks))
	for _, name := range p4AcceptanceChecks {
		checks[name] = true
	}
	payload, err := json.Marshal(p4AcceptanceClientResult{
		Schema: p4AcceptanceSchema, Success: true, Checks: checks,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeP4AcceptanceClientResult(string(payload)); err != nil {
		t.Fatalf("valid client result error = %v", err)
	}
	checks[p4AcceptanceChecks[0]] = false
	payload, _ = json.Marshal(p4AcceptanceClientResult{
		Schema: p4AcceptanceSchema, Success: true, Checks: checks,
	})
	if _, err := decodeP4AcceptanceClientResult(string(payload)); err == nil {
		t.Fatal("false successful check was accepted")
	}
	if _, err := decodeP4AcceptanceClientResult(`{"schema":"P4-ACC-001/v1","success":false,"errorCode":"MEDIA_DECODE_FAILED"}`); err != nil {
		t.Fatalf("valid failure result error = %v", err)
	}
	if _, err := decodeP4AcceptanceClientResult(`{"schema":"P4-ACC-001/v1","success":false,"errorCode":"private path"}`); err == nil {
		t.Fatal("unsafe failure code was accepted")
	}
}

func TestP4AcceptancePathsStayInsideRoot(t *testing.T) {
	root := t.TempDir()
	t.Setenv("P4ACC_ROOT", root)
	t.Setenv("P4ACC_RESULT_PATH", filepath.Join(root, "p4-acc.result.json"))
	paths, err := loadP4AcceptancePaths()
	if err != nil || paths.Root != filepath.Clean(root) || !p4AcceptancePathWithin(paths.Root, paths.ScreenshotPath) {
		t.Fatalf("paths = %+v, err = %v", paths, err)
	}
	t.Setenv("P4ACC_RESULT_PATH", filepath.Join(root, "..", "p4-acc.result.json"))
	if _, err := loadP4AcceptancePaths(); err == nil {
		t.Fatal("escaping result path was accepted")
	}
}
