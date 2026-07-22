package main

import (
	"errors"

	"github.com/jwwsjlm/douyinLive/v2/internal/analysis"
)

func (a *DesktopApp) AnalyzeSession(request analysis.AnalyzeRequest) (analysis.ReportDTO, error) {
	service, err := a.analysisService()
	if err != nil {
		return analysis.ReportDTO{}, err
	}
	return service.AnalyzeSession(a.application.Context(), request)
}

func (a *DesktopApp) GetAnalysisReport(sessionID string) (analysis.ReportDTO, error) {
	service, err := a.analysisService()
	if err != nil {
		return analysis.ReportDTO{}, err
	}
	return service.GetAnalysisReport(a.application.Context(), sessionID)
}

// GetASRStatus exposes only the provider capability state. Provider secrets,
// endpoints, models, errors, and media paths remain inside the Go process.
func (a *DesktopApp) GetASRStatus() (analysis.ASRStatusDTO, error) {
	service, err := a.analysisService()
	if err != nil {
		return analysis.ASRStatusDTO{}, err
	}
	return service.GetASRStatus(a.application.Context())
}

// ExportAnalysisReport writes a privacy-gated package beneath the application
// export root. No absolute path, media path, or source digest crosses Wails.
func (a *DesktopApp) ExportAnalysisReport(request analysis.ExportRequest) (analysis.ExportResultDTO, error) {
	service, err := a.analysisService()
	if err != nil {
		return analysis.ExportResultDTO{}, err
	}
	return service.ExportAnalysis(a.application.Context(), request)
}

func (a *DesktopApp) analysisService() (*analysis.Service, error) {
	service := a.application.AnalysisService()
	if service == nil {
		return nil, errors.New("ANALYSIS_SERVICE_UNAVAILABLE: 分析服务尚未就绪")
	}
	return service, nil
}
