package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSecurityHeadersMiddlewareLocksDesktopAssetsToSelf(t *testing.T) {
	desktop := &DesktopApp{}
	handler := desktop.securityHeadersMiddleware(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusNoContent)
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://wails.localhost/", nil))
	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
	csp := response.Header().Get("Content-Security-Policy")
	for _, required := range []string{"default-src 'self'", "object-src 'none'", "script-src 'self'", "connect-src 'self'"} {
		if !strings.Contains(csp, required) {
			t.Fatalf("CSP %q missing %q", csp, required)
		}
	}
	if strings.Contains(csp, "http:") || strings.Contains(csp, "https:") || strings.Contains(csp, "script-src 'unsafe-inline'") {
		t.Fatalf("CSP permits remote or inline scripts: %q", csp)
	}
	if got := response.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("Referrer-Policy = %q", got)
	}
}
