//go:build p2acceptance

package main

import (
	"path/filepath"
	"testing"
)

func TestLoadP2AcceptancePathsRequiresIsolatedAbsolutePaths(t *testing.T) {
	root := t.TempDir()
	validResult := filepath.Join(root, "results", "phase-1.result.json")
	validRecording := filepath.Join(root, "recordings")
	outside := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-outside")

	tests := []struct {
		name      string
		root      string
		result    string
		recording string
		wantError bool
	}{
		{name: "valid descendants", root: root, result: validResult, recording: validRecording},
		{name: "relative root", root: "relative-root", result: validResult, recording: validRecording, wantError: true},
		{name: "relative result", root: root, result: "phase-1.result.json", recording: validRecording, wantError: true},
		{name: "relative recording", root: root, result: validResult, recording: "recordings", wantError: true},
		{name: "result outside", root: root, result: filepath.Join(outside, "phase-1.result.json"), recording: validRecording, wantError: true},
		{name: "recording outside", root: root, result: validResult, recording: outside, wantError: true},
		{name: "wrong result filename", root: root, result: filepath.Join(root, "results", "unexpected.json"), recording: validRecording, wantError: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("P2ACC_ROOT", test.root)
			t.Setenv("P2ACC_RESULT_PATH", test.result)
			t.Setenv("P2ACC_RECORDING_DIRECTORY", test.recording)
			_, err := loadP2AcceptancePaths(1)
			if test.wantError && err == nil {
				t.Fatal("loadP2AcceptancePaths() error = nil, want error")
			}
			if !test.wantError && err != nil {
				t.Fatalf("loadP2AcceptancePaths() error = %v", err)
			}
		})
	}
}

func TestValidateP2AcceptanceStorageRootRequiresContainment(t *testing.T) {
	root := t.TempDir()
	if err := validateP2AcceptanceStorageRoot(root, filepath.Join(root, "data")); err != nil {
		t.Fatalf("contained storage root rejected: %v", err)
	}
	outside := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-outside")
	if err := validateP2AcceptanceStorageRoot(root, outside); err == nil {
		t.Fatal("outside storage root accepted")
	}
	if err := validateP2AcceptanceStorageRoot(root, "relative-data"); err == nil {
		t.Fatal("relative storage root accepted")
	}
}

func TestValidateP2AcceptanceResultRejectsAttentionStatusOnSuccess(t *testing.T) {
	t.Setenv("P2ACC_PHASE", "1")
	result := p2AcceptanceResult{
		Schema:  p2AcceptanceSchema,
		Phase:   1,
		Success: true,
		Checks: map[string]bool{
			"roomCreated":      true,
			"roomEdited":       true,
			"monitoringActive": true,
			"settingsSaved":    true,
		},
		StatusLabel: "需要处理",
	}
	if err := validateP2AcceptanceResult(result); err == nil {
		t.Fatal("successful result with attention status accepted")
	}
	result.StatusLabel = "正在连接"
	if err := validateP2AcceptanceResult(result); err != nil {
		t.Fatalf("healthy successful result rejected: %v", err)
	}
}
