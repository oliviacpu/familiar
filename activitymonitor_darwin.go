//go:build darwin

package main

/*
#cgo LDFLAGS: -framework ApplicationServices
#import <Cocoa/Cocoa.h>
#import <ApplicationServices/ApplicationServices.h>

static pid_t familiarActivityMonitorPID(void) {
	@autoreleasepool {
		NSArray *apps = [NSRunningApplication runningApplicationsWithBundleIdentifier:@"com.apple.ActivityMonitor"];
		return apps.count ? [apps[0] processIdentifier] : 0;
	}
}

// Activity Monitor opens on the tab in its SelectedTab setting.
static void familiarSetActivityTab(int tab) {
	CFNumberRef n = CFNumberCreate(NULL, kCFNumberIntType, &tab);
	CFPreferencesSetAppValue(CFSTR("SelectedTab"), n, CFSTR("com.apple.ActivityMonitor"));
	CFPreferencesAppSynchronize(CFSTR("com.apple.ActivityMonitor"));
	CFRelease(n);
}

// With prompt, macOS shows its own dialog pointing to System Settings.
static int familiarAXTrusted(int prompt) {
	const void *keys[] = {kAXTrustedCheckOptionPrompt};
	const void *values[] = {prompt ? kCFBooleanTrue : kCFBooleanFalse};
	CFDictionaryRef opts = CFDictionaryCreate(NULL, keys, values, 1, &kCFTypeDictionaryKeyCallBacks, &kCFTypeDictionaryValueCallBacks);
	int ok = AXIsProcessTrustedWithOptions(opts);
	CFRelease(opts);
	return ok;
}

// familiarFind returns the first element with role at or under e, searching
// depth levels down. The caller releases it.
static AXUIElementRef familiarFind(AXUIElementRef e, CFStringRef role, int depth) {
	CFTypeRef r = NULL;
	if (AXUIElementCopyAttributeValue(e, kAXRoleAttribute, &r) == kAXErrorSuccess && r) {
		Boolean match = CFEqual(r, role);
		CFRelease(r);
		if (match) return (AXUIElementRef)CFRetain(e);
	}
	if (depth == 0) return NULL;
	CFArrayRef kids = NULL;
	if (AXUIElementCopyAttributeValue(e, kAXChildrenAttribute, (CFTypeRef *)&kids) != kAXErrorSuccess || !kids) return NULL;
	AXUIElementRef found = NULL;
	for (CFIndex i = 0; i < CFArrayGetCount(kids) && !found; i++) {
		found = familiarFind((AXUIElementRef)CFArrayGetValueAtIndex(kids, i), role, depth - 1);
	}
	CFRelease(kids);
	return found;
}

// familiarPressActivityTab presses segment tab of the tab control in the
// toolbar (CPU, Memory, Energy, Disk, Network). 1 means pressed, 0 that there's
// no window with the control yet, -1 that Accessibility refused.
static int familiarPressActivityTab(pid_t pid, int tab) {
	AXUIElementRef app = AXUIElementCreateApplication(pid);
	AXUIElementSetMessagingTimeout(app, 1.0);
	CFArrayRef wins = NULL;
	AXError err = AXUIElementCopyAttributeValue(app, kAXWindowsAttribute, (CFTypeRef *)&wins);
	CFRelease(app);
	if (err == kAXErrorAPIDisabled) return -1;
	if (err != kAXErrorSuccess || !wins) return 0;
	int pressed = 0;
	for (CFIndex i = 0; i < CFArrayGetCount(wins) && !pressed; i++) {
		AXUIElementRef toolbar = familiarFind((AXUIElementRef)CFArrayGetValueAtIndex(wins, i), kAXToolbarRole, 1);
		if (!toolbar) continue;
		AXUIElementRef group = familiarFind(toolbar, kAXRadioGroupRole, 3);
		CFRelease(toolbar);
		if (!group) continue;
		CFArrayRef segments = NULL;
		if (AXUIElementCopyAttributeValue(group, kAXChildrenAttribute, (CFTypeRef *)&segments) == kAXErrorSuccess && segments) {
			if (tab < CFArrayGetCount(segments)) {
				pressed = AXUIElementPerformAction((AXUIElementRef)CFArrayGetValueAtIndex(segments, tab), kAXPressAction) == kAXErrorSuccess;
			}
			CFRelease(segments);
		}
		CFRelease(group);
	}
	CFRelease(wins);
	return pressed;
}
*/
import "C"

import (
	"log"
	"sync"
	"time"
)

// Activity Monitor's tabs, in toolbar order, which is also how SelectedTab counts.
const (
	tabCPU     = 0
	tabMemory  = 1
	tabNetwork = 4
)

var askAccessibility sync.Once

// activityMonitor opens Activity Monitor on a tab. A fresh launch reads the tab
// from its settings. One that's already running has to be switched through
// Accessibility, which macOS asks for the first time it's needed; without it,
// Activity Monitor still comes forward, on whatever tab it was on.
func activityMonitor(tab int) opener {
	return func() {
		pid := C.familiarActivityMonitorPID()
		C.familiarSetActivityTab(C.int(tab))
		launch([]string{"/usr/bin/open", "-b", "com.apple.ActivityMonitor"})
		if pid == 0 {
			return
		}
		if C.familiarAXTrusted(0) == 0 {
			askAccessibility.Do(func() {
				log.Print("asking for Accessibility, to switch Activity Monitor's tab")
				C.familiarAXTrusted(1)
			})
			return
		}
		for range 20 { // open may still be making the window
			switch C.familiarPressActivityTab(pid, C.int(tab)) {
			case 1:
				return
			case -1:
				log.Print("Accessibility refused switching Activity Monitor's tab")
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		log.Print("couldn't find Activity Monitor's tabs")
	}
}
