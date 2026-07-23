//go:build !windows

package update

import (
	"net/http"
	"net/url"
)

func systemProxy(*http.Request) (*url.URL, error) {
	return nil, nil
}
