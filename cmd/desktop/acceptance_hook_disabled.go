//go:build !p2acceptance && !p3uiacceptance

package main

import "context"

func (a *DesktopApp) startAcceptanceHook(context.Context) {}
