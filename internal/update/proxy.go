package update

import (
	"errors"
	"net/http"
	"net/url"
	"strings"
)

func defaultProxy(request *http.Request) (*url.URL, error) {
	proxyURL, err := http.ProxyFromEnvironment(request)
	if proxyURL != nil || err != nil {
		return proxyURL, err
	}
	return systemProxy(request)
}

func parseStaticProxy(raw, requestScheme string) (*url.URL, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil, nil
	}
	if isProxyMapping(value) {
		value = selectProxyForScheme(value, requestScheme)
		if value == "" {
			return nil, nil
		}
	}
	if !strings.Contains(value, "://") {
		value = "http://" + value
	}
	proxyURL, err := url.Parse(value)
	if err != nil {
		return nil, errors.New("UPDATE_SYSTEM_PROXY_INVALID")
	}
	if (proxyURL.Scheme != "http" && proxyURL.Scheme != "https") ||
		proxyURL.Host == "" || proxyURL.Hostname() == "" || proxyURL.User != nil ||
		proxyURL.Opaque != "" || (proxyURL.Path != "" && proxyURL.Path != "/") ||
		proxyURL.RawQuery != "" || proxyURL.Fragment != "" {
		return nil, errors.New("UPDATE_SYSTEM_PROXY_INVALID")
	}
	return proxyURL, nil
}

func isProxyMapping(raw string) bool {
	first := raw
	if separator := strings.IndexByte(first, ';'); separator >= 0 {
		first = first[:separator]
	}
	key, _, found := strings.Cut(first, "=")
	if !found {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "http", "https", "ftp", "socks":
		return true
	default:
		return false
	}
}

func selectProxyForScheme(raw, requestScheme string) string {
	requestScheme = strings.ToLower(strings.TrimSpace(requestScheme))
	for _, entry := range strings.Split(raw, ";") {
		key, value, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		if strings.ToLower(strings.TrimSpace(key)) == requestScheme {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
