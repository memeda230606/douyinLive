package main

import "net/http"

const desktopContentSecurityPolicy = "default-src 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'; form-action 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; media-src 'self'; connect-src 'self'"

func (a *DesktopApp) securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Security-Policy", desktopContentSecurityPolicy)
		response.Header().Set("Referrer-Policy", "no-referrer")
		response.Header().Set("X-Content-Type-Options", "nosniff")
		response.Header().Set("X-Frame-Options", "DENY")
		next.ServeHTTP(response, request)
	})
}
