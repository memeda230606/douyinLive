package app

import "github.com/jwwsjlm/douyinLive/v2/internal/analysis"

func (a *Application) AnalysisService() *analysis.Service {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.analysis
}
