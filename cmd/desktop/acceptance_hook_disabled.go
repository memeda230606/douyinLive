//go:build !p2acceptance && !p3uiacceptance && !p3accacceptance && !p4accacceptance

package main

import "context"

func (a *DesktopApp) startAcceptanceHook(context.Context) {}
