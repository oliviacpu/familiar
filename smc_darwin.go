//go:build darwin

package main

// Reading temperatures from the System Management Controller. On M3 and M4
// chips the GPU's temperature sensors are only here, as keys starting "Tg".
// This only reads key information and values; it never writes to the SMC.

/*
#cgo LDFLAGS: -framework IOKit
#include <IOKit/IOKitLib.h>
#include <mach/mach.h>
#include <string.h>

typedef struct { unsigned char major, minor, build, reserved; unsigned short release; } smcVers;
typedef struct { unsigned short version, length; unsigned int cpuPLimit, gpuPLimit, memPLimit; } smcPLimit;
typedef struct { unsigned int dataSize, dataType; unsigned char dataAttributes; } smcKeyInfo;
typedef struct {
	unsigned int key;
	smcVers vers;
	smcPLimit pLimitData;
	smcKeyInfo keyInfo;
	unsigned char result, status, data8;
	unsigned int data32;
	unsigned char bytes[32];
} smcKeyData;

enum { kSMCUserClient = 2, kSMCReadBytes = 5, kSMCReadIndex = 8, kSMCReadKeyInfo = 9 };

static io_connect_t smcConn = 0;

static int smcOpen(void) {
	if (smcConn) return 0;
	io_service_t svc = IOServiceGetMatchingService(MACH_PORT_NULL, IOServiceMatching("AppleSMC"));
	if (!svc) return -1;
	kern_return_t r = IOServiceOpen(svc, mach_task_self(), 0, &smcConn);
	IOObjectRelease(svc);
	return r == KERN_SUCCESS ? 0 : -1;
}

static int smcCall(smcKeyData *in, smcKeyData *out) {
	size_t outSize = sizeof(smcKeyData);
	memset(out, 0, sizeof(smcKeyData));
	kern_return_t r = IOConnectCallStructMethod(smcConn, kSMCUserClient, in, sizeof(smcKeyData), out, &outSize);
	return (r == KERN_SUCCESS && out->result == 0) ? 0 : -1;
}

static int smcKeyAt(unsigned int index, unsigned int *key) {
	smcKeyData in, out;
	memset(&in, 0, sizeof(in));
	in.data8 = kSMCReadIndex;
	in.data32 = index;
	if (smcCall(&in, &out) != 0) return -1;
	*key = out.key;
	return 0;
}

static int smcInfo(unsigned int key, unsigned int *size, unsigned int *type) {
	smcKeyData in, out;
	memset(&in, 0, sizeof(in));
	in.key = key;
	in.data8 = kSMCReadKeyInfo;
	if (smcCall(&in, &out) != 0) return -1;
	*size = out.keyInfo.dataSize;
	*type = out.keyInfo.dataType;
	return 0;
}

static int smcRead(unsigned int key, unsigned int size, unsigned char *buf) {
	smcKeyData in, out;
	if (size > 32) return -1;
	memset(&in, 0, sizeof(in));
	in.key = key;
	in.keyInfo.dataSize = size;
	in.data8 = kSMCReadBytes;
	if (smcCall(&in, &out) != 0) return -1;
	memcpy(buf, out.bytes, size);
	return 0;
}
*/
import "C"

import (
	"strings"
	"sync"
	"unsafe"
)

type smcKey struct {
	name       string
	key        uint32
	size, kind uint32
}

var (
	smcOnce          sync.Once
	smcGPU, smcOther []smcKey // GPU sensors, and the other temperature keys (for `familiar temps`)
)

// smcScan lists every SMC key once and keeps the temperature ones.
func smcScan() {
	if C.smcOpen() != 0 {
		return
	}
	var buf [32]byte
	countKey := C.uint(fourCC("#KEY"))
	var size, kind C.uint
	if C.smcInfo(countKey, &size, &kind) != 0 || size != 4 ||
		C.smcRead(countKey, 4, (*C.uchar)(unsafe.Pointer(&buf[0]))) != 0 {
		return
	}
	count := uint32(buf[0])<<24 | uint32(buf[1])<<16 | uint32(buf[2])<<8 | uint32(buf[3])
	for i := uint32(0); i < count && i < 10000; i++ {
		var k C.uint
		if C.smcKeyAt(C.uint(i), &k) != 0 {
			continue
		}
		name := fourCCString(uint32(k))
		if !strings.HasPrefix(name, "T") {
			continue
		}
		if C.smcInfo(k, &size, &kind) != 0 {
			continue
		}
		t := fourCCString(uint32(kind))
		if !(t == "flt " && size == 4) && !(t == "sp78" && size == 2) {
			continue
		}
		sk := smcKey{name: name, key: uint32(k), size: uint32(size), kind: uint32(kind)}
		if strings.HasPrefix(name, "Tg") {
			smcGPU = append(smcGPU, sk)
		} else {
			smcOther = append(smcOther, sk)
		}
	}
}

func smcReadTemp(k smcKey) (float64, bool) {
	var buf [32]byte
	if C.smcRead(C.uint(k.key), C.uint(k.size), (*C.uchar)(unsafe.Pointer(&buf[0]))) != 0 {
		return 0, false
	}
	return decodeSMCTemp(k.kind, buf[:k.size])
}

func smcRead(keys []smcKey) []float64 {
	var out []float64
	for _, k := range keys {
		if v, ok := smcReadTemp(k); ok {
			out = append(out, v)
		}
	}
	return out
}

// smcGPUTemp is the average of the GPU's SMC sensors, or nil if there are none.
func smcGPUTemp() *float64 {
	smcOnce.Do(smcScan)
	return averagePlausible(smcRead(smcGPU))
}

// smcAllTemps is every SMC temperature key and its value, for `familiar temps`.
func smcAllTemps() (gpu, other map[string]float64) {
	smcOnce.Do(smcScan)
	gpu, other = map[string]float64{}, map[string]float64{}
	for _, k := range smcGPU {
		if v, ok := smcReadTemp(k); ok {
			gpu[k.name] = v
		}
	}
	for _, k := range smcOther {
		if v, ok := smcReadTemp(k); ok {
			other[k.name] = v
		}
	}
	return gpu, other
}
