package douyinLive

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
)

const proxyProbeEnvironment = "DOUYINLIVE_PROXY_PROBE"

func TestNewHTTPClientUsesEnvironmentProxy(t *testing.T) {
	if proxyURL := os.Getenv(proxyProbeEnvironment); proxyURL != "" {
		t.Setenv("HTTP_PROXY", proxyURL)
		t.Setenv("HTTPS_PROXY", proxyURL)
		t.Setenv("NO_PROXY", "")
		response, err := newHTTPClient("proxy-probe").R().Get("http://proxy-target.invalid/probe")
		if err != nil {
			t.Fatalf("request through environment proxy: %v", err)
		}
		if response.GetStatusCode() != http.StatusNoContent {
			t.Fatalf("proxy response status = %d", response.GetStatusCode())
		}
		return
	}

	proxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Host != "proxy-target.invalid" || request.URL.Path != "/probe" {
			http.Error(writer, "unexpected proxy target", http.StatusBadGateway)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer proxy.Close()

	command := exec.Command(os.Args[0], "-test.run=^TestNewHTTPClientUsesEnvironmentProxy$")
	command.Env = append(withoutProxyEnvironment(os.Environ()), proxyProbeEnvironment+"="+proxy.URL)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("proxy subprocess failed: %v\n%s", err, output)
	}
}

func TestProxyEnvironmentConfigured(t *testing.T) {
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(name, "")
	}
	if proxyEnvironmentConfigured() {
		t.Fatal("empty proxy environment reported configured")
	}
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	if !proxyEnvironmentConfigured() {
		t.Fatal("HTTPS_PROXY was ignored")
	}
}

func withoutProxyEnvironment(environment []string) []string {
	blocked := map[string]struct{}{
		"HTTP_PROXY": {}, "HTTPS_PROXY": {}, "NO_PROXY": {},
		proxyProbeEnvironment: {},
	}
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		name, _, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		if _, excluded := blocked[strings.ToUpper(name)]; excluded {
			continue
		}
		result = append(result, entry)
	}
	return append(result, "NO_PROXY=")
}
