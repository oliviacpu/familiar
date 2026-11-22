//go:build darwin

package main

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework Cocoa
#import <Cocoa/Cocoa.h>

// An accessory app has a menu bar item but no Dock icon and no app menu.
static void familiarHideDockIcon(void) {
	[NSApplication sharedApplication];
	[NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
}
*/
import "C"

import (
	"context"
	"strings"
	"sync"
	"time"
)

func hideDockIcon() { C.familiarHideDockIcon() }

func trayHost() bool { return true }

func prepareTray(context.Context) {} // the menu bar is there before any login item starts

var iconTray []byte // unused: the menu bar takes the template image

var unitOnce sync.Once
var unit = "C"

// tempUnit follows System Settings › General › Language & Region › Temperature.
func tempUnit() string {
	unitOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if out, err := runQuiet(ctx, "/usr/bin/defaults", "read", "-g", "AppleTemperatureUnit"); err == nil && strings.HasPrefix(strings.TrimSpace(out), "Fahrenheit") {
			unit = "F"
		}
	})
	return unit
}
