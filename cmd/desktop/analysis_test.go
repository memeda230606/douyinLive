package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/jwwsjlm/douyinLive/v2/internal/analysis"
	application "github.com/jwwsjlm/douyinLive/v2/internal/app"
)

func TestDesktopAnalysisFacadeRequiresServiceAndPreservesStableErrors(t *testing.T) {
	applicationService := application.New(application.Options{})
	desktop := NewDesktopApp(applicationService)
	if _, err := desktop.GetAnalysisReport("not-ready"); err == nil || err.Error() != "ANALYSIS_SERVICE_UNAVAILABLE: 分析服务尚未就绪" {
		t.Fatalf("unavailable error = %v", err)
	}
	if _, err := desktop.GetASRStatus(); err == nil || err.Error() != "ANALYSIS_SERVICE_UNAVAILABLE: 分析服务尚未就绪" {
		t.Fatalf("unavailable ASR error = %v", err)
	}
	if err := applicationService.InitializeInfrastructure(context.Background(), application.InfrastructureOptions{
		DataRoot: filepath.Join(t.TempDir(), "analysis-desktop-data"), DisableDiagnostics: true,
	}); err != nil {
		t.Fatalf("InitializeInfrastructure() error = %v", err)
	}
	t.Cleanup(func() { _ = applicationService.Shutdown(context.Background()) })
	if _, err := desktop.AnalyzeSession(analysis.AnalyzeRequest{SessionID: "invalid"}); !errors.Is(err, analysis.ErrInvalidArgument) {
		t.Fatalf("invalid request error = %v", err)
	}
	status, err := desktop.GetASRStatus()
	if err != nil {
		t.Fatalf("GetASRStatus() error = %v", err)
	}
	if status.State != analysis.ASRStateDisabled || status.ErrorCode != analysis.ASRNotConfiguredErrorCode {
		t.Fatalf("GetASRStatus() = %+v", status)
	}
}
