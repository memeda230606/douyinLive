//go:build windows

package update

import (
	"errors"
	"net/http"
	"net/url"

	"golang.org/x/sys/windows/registry"
)

const windowsInternetSettingsKey = `Software\Microsoft\Windows\CurrentVersion\Internet Settings`

func systemProxy(request *http.Request) (*url.URL, error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, windowsInternetSettingsKey, registry.QUERY_VALUE)
	if errors.Is(err, registry.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("UPDATE_SYSTEM_PROXY_UNAVAILABLE")
	}
	defer key.Close()

	enabled, _, err := key.GetIntegerValue("ProxyEnable")
	if errors.Is(err, registry.ErrNotExist) || enabled == 0 {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("UPDATE_SYSTEM_PROXY_UNAVAILABLE")
	}
	proxyServer, _, err := key.GetStringValue("ProxyServer")
	if errors.Is(err, registry.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.New("UPDATE_SYSTEM_PROXY_UNAVAILABLE")
	}
	return parseStaticProxy(proxyServer, request.URL.Scheme)
}
