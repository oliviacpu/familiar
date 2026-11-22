//go:build darwin

package main

// NVMe drives (every Mac SSD since 2015) report their SMART / health log
// through Apple's NVMeSMARTLib plug-in, the interface smartmontools uses
// (os_darwin.h, "NVMe definitions based on Xcode SDK"). Read only.

/*
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation
#include <IOKit/IOKitLib.h>
#include <IOKit/IOCFPlugIn.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdlib.h>
#include <string.h>

#define kNVMeSMARTUserClientTypeID CFUUIDGetConstantUUIDWithBytes(NULL, \
	0xAA, 0x0F, 0xA6, 0xF9, 0xC2, 0xD6, 0x45, 0x7F, 0xB1, 0x0B, 0x59, 0xA1, 0x32, 0x53, 0x29, 0x2F)
#define kNVMeSMARTInterfaceID CFUUIDGetConstantUUIDWithBytes(NULL, \
	0xcc, 0xd1, 0xdb, 0x19, 0xfd, 0x9a, 0x4d, 0xaf, 0xbf, 0x95, 0x12, 0x45, 0x4b, 0x23, 0x0a, 0xb6)

typedef struct NVMeSMARTInterface {
	IUNKNOWN_C_GUTS;
	UInt16 version;
	UInt16 revision;
	IOReturn (*SMARTReadData)(void *self, void *log512);
	IOReturn (*GetIdentifyData)(void *self, void *id4096, unsigned int ns);
	UInt64 reserved0;
	UInt64 reserved1;
	IOReturn (*GetLogPage)(void *self, void *data, unsigned int logPageId, unsigned int numDWords);
} NVMeSMARTInterface;

// nvmeLog reads the 512-byte health log of the drive behind BSD name bsd
// ("disk0"). Returns 0 on success, a negative step number otherwise.
static int nvmeLog(const char *bsd, unsigned char *out) {
	io_service_t disk = IOServiceGetMatchingService(kIOMainPortDefault, IOBSDNameMatching(kIOMainPortDefault, 0, bsd));
	if (!disk) return -1;
	// Walk up to the controller that says it can do NVMe SMART.
	for (;;) {
		CFTypeRef cap = IORegistryEntryCreateCFProperty(disk, CFSTR("NVMe SMART Capable"), kCFAllocatorDefault, 0);
		if (cap) { CFRelease(cap); break; }
		io_registry_entry_t parent = 0;
		kern_return_t r = IORegistryEntryGetParentEntry(disk, kIOServicePlane, &parent);
		IOObjectRelease(disk);
		if (r != KERN_SUCCESS || !parent) return -2;
		disk = parent;
	}
	IOCFPlugInInterface **plugin = NULL;
	SInt32 score = 0;
	kern_return_t r = IOCreatePlugInInterfaceForService(disk, kNVMeSMARTUserClientTypeID, kIOCFPlugInInterfaceID, &plugin, &score);
	IOObjectRelease(disk);
	if (r != KERN_SUCCESS || !plugin) return -3;
	NVMeSMARTInterface **smart = NULL;
	HRESULT hr = (*plugin)->QueryInterface(plugin, CFUUIDGetUUIDBytes(kNVMeSMARTInterfaceID), (LPVOID *)&smart);
	if (hr != S_OK || !smart) { IODestroyPlugInInterface(plugin); return -4; }
	memset(out, 0, 512);
	IOReturn e = (*smart)->SMARTReadData(smart, out);
	(*smart)->Release(smart);
	IODestroyPlugInInterface(plugin);
	return e == kIOReturnSuccess ? 0 : -5;
}
*/
import "C"

import (
	"context"
	"encoding/json"
	"os"
	"time"
	"unsafe"
)

// readDriveHealth runs `familiar drives --json` and reads its answer, so a
// fault inside Apple's plug-in can't stop the reporter itself.
func readDriveHealth(ctx context.Context) []DriveHealth {
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := runQuiet(ctx, exe, "drives", "--json")
	if err != nil {
		return nil
	}
	var ds []DriveHealth
	if json.Unmarshal([]byte(out), &ds) != nil {
		return nil
	}
	return ds
}

// collectDriveHealth does the reading, in the `familiar drives` process.
func collectDriveHealth(ctx context.Context) []DriveHealth {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	list, err := runQuiet(ctx, "/usr/sbin/diskutil", "list", "physical")
	if err != nil {
		return nil
	}
	var ds []DriveHealth
	for _, id := range parseDiskutilList(list) {
		info, _ := runQuiet(ctx, "/usr/sbin/diskutil", "info", id)
		d := parseDiskutilInfo(id, info)
		buf := make([]byte, 512)
		cs := C.CString(id)
		rc := C.nvmeLog(cs, (*C.uchar)(unsafe.Pointer(&buf[0])))
		C.free(unsafe.Pointer(cs))
		if rc == 0 {
			verdict := d // diskutil's SMART verdict
			readNVMeLog(&d, buf)
			if verdict.Problem && d.Problem {
				d.Status = verdict.Status + ", " + d.Status
			} else if verdict.Problem {
				d.Status, d.Problem = verdict.Status, true
			}
		}
		ds = append(ds, d)
	}
	return ds
}
