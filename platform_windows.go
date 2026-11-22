//go:build windows

package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// The Windows build is a GUI program so that running at login opens no
// window. From a terminal it borrows that terminal's console for its output.
func attachConsole() {
	attach := windows.NewLazySystemDLL("kernel32.dll").NewProc("AttachConsole")
	if r, _, _ := attach.Call(uintptr(^uint32(0))); r == 0 { // ATTACH_PARENT_PROCESS
		return
	}
	name, _ := windows.UTF16PtrFromString("CONOUT$")
	h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return
	}
	f := os.NewFile(uintptr(h), "CONOUT$")
	os.Stdout, os.Stderr = f, f
	fmt.Println()
}

// runQuiet runs a helper tool without flashing a console window.
func runQuiet(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	out, err := cmd.Output()
	return string(out), err
}

func nvidiaSMI() string {
	if p, err := exec.LookPath("nvidia-smi"); err == nil {
		return p
	}
	for _, p := range []string{`C:\Windows\System32\nvidia-smi.exe`, `C:\Program Files\NVIDIA Corporation\NVSMI\nvidia-smi.exe`} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// readGPU uses NVIDIA's tool when there is one (load and temperature), and
// otherwise Windows' own GPU counters, which give load only.
func readGPU(ctx context.Context) (util, temp *float64) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if smi := nvidiaSMI(); smi != "" {
		if out, err := runQuiet(ctx, smi, "--query-gpu=utilization.gpu,temperature.gpu", "--format=csv,noheader,nounits"); err == nil {
			if u, t := parseNvidiaSMI(out); u != nil {
				return u, t
			}
		}
	}
	out, err := runQuiet(ctx, "typeperf", `\GPU Engine(*engtype_3D)\Utilization Percentage`, "-sc", "2")
	if err != nil {
		return nil, nil
	}
	return parseTypeperf(out), nil
}

func readBattery(context.Context) (*float64, *bool) { return nil, nil }

func hideDockIcon() {} // nothing to hide: the Windows build has no window or taskbar button

// tempUnit follows the region's measurement system (US means °F).
func tempUnit() string {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Control Panel\International`, registry.QUERY_VALUE)
	if err != nil {
		return "C"
	}
	defer k.Close()
	if v, _, err := k.GetStringValue("iMeasure"); err == nil && v == "1" {
		return "F"
	}
	return "C"
}

// stopOthers ends any other running familiar.exe, but not this one.
func stopOthers() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = runQuiet(ctx, "taskkill", "/F", "/IM", filepath.Base(os.Args[0]), "/FI", fmt.Sprintf("PID ne %d", os.Getpid()))
}

func logPath() string {
	dir, err := os.UserCacheDir() // %LocalAppData%
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "familiar", "familiar.log")
}

// setupLogging writes to a log file, since at login there's no console.
func setupLogging() {
	p := logPath()
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	if fi, err := os.Stat(p); err == nil && fi.Size() > 1<<20 {
		_ = os.Remove(p)
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	log.SetOutput(io.MultiWriter(f, os.Stderr))
}

// isLocalDrive is true for drives built into this PC: fixed, and not on a
// removable bus (USB, SD), since Windows calls many USB drives fixed.
func isLocalDrive(mount string) bool {
	p, err := windows.UTF16PtrFromString(strings.TrimRight(mount, `\`) + `\`)
	if err != nil || windows.GetDriveType(p) != windows.DRIVE_FIXED {
		return false
	}
	return !removableVolume(strings.TrimRight(mount, `\`))
}

// isMountPoint: a listed drive letter that isn't there fails to read anyway.
func isMountPoint(string) bool { return true }
