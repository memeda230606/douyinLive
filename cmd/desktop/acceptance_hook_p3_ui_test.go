//go:build p3uiacceptance

package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jwwsjlm/douyinLive/v2/internal/eventstore"
	"github.com/jwwsjlm/douyinLive/v2/internal/room"
)

func TestLoadP3UIAcceptancePathsRequiresIsolation(t *testing.T) {
	root := t.TempDir()
	validResult := filepath.Join(root, "results", "p3-ui.result.json")
	outside := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-outside")
	tests := []struct {
		name, root, result string
		wantError          bool
	}{
		{name: "valid descendant", root: root, result: validResult},
		{name: "relative root", root: "relative", result: validResult, wantError: true},
		{name: "relative result", root: root, result: "p3-ui.result.json", wantError: true},
		{name: "outside result", root: root, result: filepath.Join(outside, "p3-ui.result.json"), wantError: true},
		{name: "wrong filename", root: root, result: filepath.Join(root, "result.json"), wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("P3UIACC_ROOT", test.root)
			t.Setenv("P3UIACC_RESULT_PATH", test.result)
			_, err := loadP3UIAcceptancePaths()
			if test.wantError && err == nil {
				t.Fatal("loadP3UIAcceptancePaths() error = nil, want error")
			}
			if !test.wantError && err != nil {
				t.Fatalf("loadP3UIAcceptancePaths() error = %v", err)
			}
		})
	}
}

func TestValidateP3UIAcceptanceStorageRootRequiresContainment(t *testing.T) {
	root := t.TempDir()
	if err := validateP3UIAcceptanceStorageRoot(root, filepath.Join(root, "data")); err != nil {
		t.Fatalf("contained storage root rejected: %v", err)
	}
	outside := filepath.Join(filepath.Dir(root), filepath.Base(root)+"-outside")
	if err := validateP3UIAcceptanceStorageRoot(root, outside); err == nil {
		t.Fatal("outside storage root accepted")
	}
}

func TestValidateP3UIAcceptanceResultIsStrict(t *testing.T) {
	checks := make(map[string]bool, len(p3UIAcceptanceChecks))
	for _, name := range p3UIAcceptanceChecks {
		checks[name] = true
	}
	valid := p3UIAcceptanceResult{
		Schema: p3UIAcceptanceSchema, Success: true, Checks: checks,
		VisibleEventCount: 2000, FilteredEventCount: 333,
		AlertCount: 2, StatusLabel: "录制中",
	}
	if err := validateP3UIAcceptanceResult(valid); err != nil {
		t.Fatalf("valid result rejected: %v", err)
	}
	invalid := valid
	invalid.Checks = map[string]bool{"unexpected": true}
	if err := validateP3UIAcceptanceResult(invalid); err == nil {
		t.Fatal("unknown check accepted")
	}
	invalid = valid
	invalid.FilteredEventCount = invalid.VisibleEventCount
	if err := validateP3UIAcceptanceResult(invalid); err == nil {
		t.Fatal("non-reducing event filter accepted")
	}
	if err := validateP3UIAcceptanceResult(p3UIAcceptanceResult{
		Schema: p3UIAcceptanceSchema, ErrorCode: "invalid-code",
	}); err == nil {
		t.Fatal("invalid failure code accepted")
	}
}

func TestP3UIAcceptanceFixtureUsesBoundedPrivacySafeDTOs(t *testing.T) {
	root := t.TempDir()
	t.Setenv("P3UIACC_ROOT", root)
	t.Setenv("P3UIACC_RESULT_PATH", filepath.Join(root, "results", "p3-ui.result.json"))
	var emitted []any
	app := &DesktopApp{
		emitEvent: func(_ context.Context, _ string, values ...interface{}) {
			if len(values) == 1 {
				emitted = append(emitted, values[0])
			}
		},
	}
	app.acceptingEvents.Store(true)
	state := &p3UIAcceptanceState{
		ctx:   context.Background(),
		paths: p3UIAcceptancePaths{Root: root, ResultPath: filepath.Join(root, "results", "p3-ui.result.json")},
		room: room.RoomConfig{
			ID:     "018f47a0-7c00-7000-8000-000000000020",
			LiveID: p3UIAcceptanceLiveID, Alias: p3UIAcceptanceAlias,
		},
		baseAt: 1_720_000_000_000, nextStage: 1,
	}
	p3UIAcceptanceRegistry.Lock()
	p3UIAcceptanceRegistry.states[app] = state
	p3UIAcceptanceRegistry.Unlock()
	t.Cleanup(func() {
		p3UIAcceptanceRegistry.Lock()
		delete(p3UIAcceptanceRegistry.states, app)
		p3UIAcceptanceRegistry.Unlock()
	})

	if err := app.EmitP3UIAcceptanceFixture(1); err != nil {
		t.Fatalf("stage 1 failed: %v", err)
	}
	if err := app.EmitP3UIAcceptanceFixture(1); err == nil {
		t.Fatal("duplicate stage accepted")
	}
	if err := app.EmitP3UIAcceptanceFixture(2); err != nil {
		t.Fatalf("stage 2 failed: %v", err)
	}
	if err := app.EmitP3UIAcceptanceFixture(3); err != nil {
		t.Fatalf("stage 3 failed: %v", err)
	}

	var liveEvents int
	for _, payload := range emitted {
		if batch, ok := payload.(eventstore.LiveEventBatchDTO); ok {
			if len(batch.Events) == 0 || len(batch.Events) > 100 {
				t.Fatalf("live batch size = %d", len(batch.Events))
			}
			liveEvents += len(batch.Events)
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal fixture payload: %v", err)
		}
		text := string(encoded)
		for _, forbidden := range []string{"userHash", "dedupeKey", "normalizedJSON", "rawFile", "http://", "https://", `C:\\`} {
			if strings.Contains(text, forbidden) {
				t.Fatalf("fixture payload contains forbidden marker %q", forbidden)
			}
		}
	}
	if liveEvents != p3UIAcceptanceEventCount+1 {
		t.Fatalf("live event count = %d, want %d", liveEvents, p3UIAcceptanceEventCount+1)
	}
}
