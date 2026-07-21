package app

import (
	"context"
	"path/filepath"
	"testing"
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
