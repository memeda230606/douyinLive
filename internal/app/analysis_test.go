package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/jwwsjlm/douyinLive/v2/internal/analysis"
)

func TestInitializeInfrastructureProvidesAnalysisService(t *testing.T) {
	application := New(Options{Name: "test", Version: "test"})
	if err := application.InitializeInfrastructure(
		context.Background(),
		InfrastructureOptions{
			DataRoot:           filepath.Join(t.TempDir(), "analysis-app-data"),
			DisableDiagnostics: true,
		},
	); err != nil {
		t.Fatalf("InitializeInfrastructure() error = %v", err)
	}
	if application.AnalysisService() == nil {
		t.Fatal("AnalysisService() is nil")
	}
	status, err := application.AnalysisService().GetASRStatus(context.Background())
	if err != nil {
		t.Fatalf("GetASRStatus() error = %v", err)
	}
	if status.ProviderID != analysis.DisabledASRProviderID || status.ErrorCode != analysis.ASRNotConfiguredErrorCode {
		t.Fatalf("default ASR status = %+v", status)
	}
	available := false
	for _, capability := range application.Bootstrap().Capabilities {
		if capability.ID == "analysis" {
			available = capability.Available
		}
	}
	if !available {
		t.Fatal("analysis capability is unavailable")
	}
	application.Startup(context.Background())
	if err := application.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if application.AnalysisService() != nil {
		t.Fatal("AnalysisService() remained available after shutdown")
	}
}

type readyASRProvider struct{}

func (readyASRProvider) ID() string                     { return "local-test" }
func (readyASRProvider) Validate(context.Context) error { return nil }
func (readyASRProvider) Transcribe(
	context.Context,
	analysis.AudioInput,
	analysis.ProgressFunc,
) ([]analysis.TranscriptSegment, error) {
	return nil, errors.New("not implemented by test adapter")
}

func TestInitializeInfrastructureAcceptsReplaceableASRProvider(t *testing.T) {
	application := New(Options{Name: "test", Version: "test"})
	if err := application.InitializeInfrastructure(
		context.Background(),
		InfrastructureOptions{
			DataRoot:           filepath.Join(t.TempDir(), "analysis-provider-data"),
			DisableDiagnostics: true,
			ASRProvider:        readyASRProvider{},
		},
	); err != nil {
		t.Fatalf("InitializeInfrastructure() error = %v", err)
	}
	t.Cleanup(func() { _ = application.Shutdown(context.Background()) })
	status, err := application.AnalysisService().GetASRStatus(context.Background())
	if err != nil {
		t.Fatalf("GetASRStatus() error = %v", err)
	}
	if status.ProviderID != "local-test" || status.State != analysis.ASRStateReady ||
		!status.Configured || !status.Available || status.ErrorCode != "" {
		t.Fatalf("replaceable ASR status = %+v", status)
	}
}
