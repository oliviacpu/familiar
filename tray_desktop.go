//go:build darwin || windows

package main

import "context"

func runReporter(ctx context.Context, a *agent) error { return runTray(ctx, a) }
