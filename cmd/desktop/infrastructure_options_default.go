//go:build !p3accacceptance

package main

import application "github.com/jwwsjlm/douyinLive/v2/internal/app"

func desktopInfrastructureOptions() (application.InfrastructureOptions, error) {
	return application.InfrastructureOptions{}, nil
}
