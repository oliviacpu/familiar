//go:build windows

package main

// The CPU temperature on Windows. Windows gives no ordinary program access to
// it; it lives in CPU registers only kernel code can read. familiar asks the
// signed PawnIO driver, with PawnIO's official modules (LGPL, see
// pawnio/COPYING), which allow reading only a fixed list of registers.
// PawnIO only lets administrators in, so this works when familiar runs from
// its scheduled task with highest privileges.

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/klauspost/cpuid/v2"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

//go:embed pawnio/IntelMSR.bin
var pawnIntelMSR []byte

//go:embed pawnio/AMDFamily17.bin
var pawnAMDFamily17 []byte

const (
	pawnDeviceType      = 41394 << 16
	ioctlPawnLoadBinary = pawnDeviceType | 0x821<<2
	ioctlPawnExecute    = pawnDeviceType | 0x841<<2

	msrTemperatureTarget = 0x1A2
	msrPackageThermalSts = 0x1B1
	smnThmTconCurTmp     = 0x00059800
)

type pawnModule struct{ h windows.Handle }

func openPawnModule(bin []byte) (*pawnModule, error) {
	name, _ := windows.UTF16PtrFromString(`\\?\GLOBALROOT\Device\PawnIO`)
	h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("opening PawnIO: %w", err)
	}
	var n uint32
	if err := windows.DeviceIoControl(h, ioctlPawnLoadBinary, &bin[0], uint32(len(bin)), nil, 0, &n, nil); err != nil {
		windows.CloseHandle(h)
		return nil, fmt.Errorf("loading the PawnIO module: %w", err)
	}
	return &pawnModule{h: h}, nil
}

func (m *pawnModule) call(fn string, arg int64) (uint64, error) {
	in := pawnioInput(fn, arg)
	out := make([]byte, 8)
	var n uint32
	if err := windows.DeviceIoControl(m.h, ioctlPawnExecute, &in[0], uint32(len(in)), &out[0], uint32(len(out)), &n, nil); err != nil {
		return 0, err
	}
	var v uint64
	for i := 7; i >= 0; i-- {
		v = v<<8 | uint64(out[i])
	}
	return v, nil
}

var (
	cpuTempOnce   sync.Once
	cpuTempRead   func() (float64, error)
	cpuTempReason string // why there's no CPU temperature, for `familiar temps`
)

func isElevated() bool { return windows.GetCurrentProcessToken().IsElevated() }

// pawnioVersion is the installed PawnIO's version, or "" if it isn't installed.
func pawnioVersion() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows\CurrentVersion\Uninstall\PawnIO`,
		registry.QUERY_VALUE|registry.WOW64_64KEY)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, _ := k.GetStringValue("DisplayVersion")
	return v
}

func setupCPUTemp() {
	switch {
	case !isElevated():
		cpuTempReason = "familiar isn't running as administrator (run familiar install)"
		return
	case !pawnioVersionOK(pawnioVersion()):
		cpuTempReason = "PawnIO 2 or later isn't installed (run familiar install)"
		return
	}
	switch {
	case cpuid.CPU.VendorID == cpuid.Intel:
		m, err := openPawnModule(pawnIntelMSR)
		if err != nil {
			cpuTempReason = err.Error()
			return
		}
		target, err := m.call("ioctl_read_msr", msrTemperatureTarget)
		if err != nil {
			cpuTempReason = "reading TjMax: " + err.Error()
			return
		}
		cpuTempRead = func() (float64, error) {
			therm, err := m.call("ioctl_read_msr", msrPackageThermalSts)
			if err != nil {
				return 0, err
			}
			if t, ok := intelPackageTemp(target, therm); ok {
				return t, nil
			}
			return 0, errors.New("implausible reading")
		}
	case cpuid.CPU.VendorID == cpuid.AMD && cpuid.CPU.Family >= 0x17:
		m, err := openPawnModule(pawnAMDFamily17)
		if err != nil {
			cpuTempReason = err.Error()
			return
		}
		name := cpuid.CPU.BrandName
		cpuTempRead = func() (float64, error) {
			raw, err := m.call("ioctl_read_smn", smnThmTconCurTmp)
			if err != nil {
				return 0, err
			}
			return amdTctl(uint32(raw), name), nil
		}
	default:
		cpuTempReason = fmt.Sprintf("no PawnIO module for this CPU (%s, family %#x)", cpuid.CPU.BrandName, cpuid.CPU.Family)
	}
}

// readCPUTemp is the CPU package temperature in °C, or nil without PawnIO.
func readCPUTemp(context.Context) *float64 {
	cpuTempOnce.Do(setupCPUTemp)
	if cpuTempRead == nil {
		return nil
	}
	t, err := cpuTempRead()
	if err != nil || t <= 0 || t >= 125 {
		return nil
	}
	return &t
}

func printPlatformTemps() {
	v := pawnioVersion()
	if v == "" {
		v = "not installed"
	}
	fmt.Printf("CPU: %s | PawnIO: %s | running as administrator: %v\n", strings.TrimSpace(cpuid.CPU.BrandName), v, isElevated())
	if t := readCPUTemp(context.Background()); t != nil {
		fmt.Printf("CPU temperature from PawnIO: %.1f °C\n", *t)
	} else {
		fmt.Println("No CPU temperature:", cpuTempReason)
	}
}
