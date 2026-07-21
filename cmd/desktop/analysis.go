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

func (a *DesktopApp) analysisService() (*analysis.Service, error) {
	service := a.application.AnalysisService()
	if service == nil {
		return nil, errors.New("ANALYSIS_SERVICE_UNAVAILABLE: 分析服务尚未就绪")
	}
	return service, nil
}
