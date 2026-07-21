//go:build !p3accacceptance || !windows

package main

import windowsoptions "github.com/wailsapp/wails/v2/pkg/options/windows"

func desktopWindowsOptions() (*windowsoptions.Options, error) {
	return nil, nil
}
