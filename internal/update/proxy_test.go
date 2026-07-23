package update

import "testing"

func TestParseStaticProxy(t *testing.T) {
	tests := []struct {
		name          string
		raw           string
		requestScheme string
		want          string
	}{
		{name: "single host and port", raw: "127.0.0.1:7890", requestScheme: "https", want: "http://127.0.0.1:7890"},
		{name: "per protocol", raw: "http=proxy.example:8080;https=secure.example:8443;socks=ignored.example:1080", requestScheme: "https", want: "http://secure.example:8443"},
		{name: "explicit HTTPS proxy", raw: "https://proxy.example:8443", requestScheme: "https", want: "https://proxy.example:8443"},
		{name: "missing protocol mapping", raw: "http=proxy.example:8080", requestScheme: "https", want: ""},
		{name: "blank", raw: " ", requestScheme: "https", want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			proxyURL, err := parseStaticProxy(test.raw, test.requestScheme)
			if err != nil {
				t.Fatal(err)
			}
			if test.want == "" {
				if proxyURL != nil {
					t.Fatalf("proxy = %q, want nil", proxyURL)
				}
				return
			}
			if proxyURL == nil || proxyURL.String() != test.want {
				t.Fatalf("proxy = %v, want %q", proxyURL, test.want)
			}
		})
	}
}

func TestParseStaticProxyRejectsUnsafeValues(t *testing.T) {
	values := []string{
		"ftp://proxy.example:21",
		"http://user:password@proxy.example:8080",
		"http://proxy.example:8080/path",
		"http://proxy.example:8080?token=value",
		"http://proxy.example:8080#fragment",
	}
	for _, value := range values {
		t.Run(value, func(t *testing.T) {
			if _, err := parseStaticProxy(value, "https"); err == nil {
				t.Fatal("expected invalid proxy error")
			}
		})
	}
}
