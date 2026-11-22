//go:build darwin

package main

import (
	"context"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

func attachConsole() {}

func setupLogging() {} // launchd sends output to ~/Library/Logs/familiar.log

func runQuiet(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

// readGPU reads the GPU's load from IOKit and its temperature from the SMC's
// "Tg" sensors (where M3 and M4 keep it). Without those, collect.go falls back
// to HID sensors named "GPU…" (M1, M2) or the SMC keys gopsutil knows (Intel).
func readGPU(ctx context.Context) (util, temp *float64) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	temp = smcGPUTemp()
	out, err := runQuiet(ctx, "/usr/sbin/ioreg", "-r", "-d", "1", "-w", "0", "-c", "IOAccelerator")
	if err != nil {
		return nil, temp
	}
	return parseIoregGPU(out), temp
}

func readCPUTemp(context.Context) *float64 { return nil } // the HID sensors cover it

func printPlatformTemps() {
	gpu, other := smcAllTemps()
	if len(gpu)+len(other) == 0 {
		fmt.Println("SMC: no temperature keys readable.")
		return
	}
	fmt.Printf("SMC GPU sensors (%d, averaged):\n", len(gpu))
	for _, k := range sortedKeys(gpu) {
		fmt.Printf("  %-8s %6.1f °C\n", k, gpu[k])
	}
	fmt.Printf("SMC other temperature keys: %d\n", len(other))
}

func readBattery(ctx context.Context) (*float64, *bool) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := runQuiet(ctx, "/usr/bin/pmset", "-g", "batt")
	if err != nil {
		return nil, nil
	}
	return parsePmset(out)
}

// What clicking a reading in the menu opens.
var (
	openCPU     = activityMonitor(tabCPU)
	openMemory  = activityMonitor(tabMemory)
	openGPU     = openCPU // the CPU tab has the % GPU and GPU Time columns
	openNetwork = activityMonitor(tabNetwork)
	openStorage = launcher("/usr/bin/open", "x-apple.systempreferences:com.apple.settings.Storage")
	openBattery = launcher("/usr/bin/open", "x-apple.systempreferences:com.apple.Battery-Settings.extension") // Battery Health is here
)

const launchLabel = "zone.olivia.familiar"

func plistPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", launchLabel+".plist")
}

// install adds a LaunchAgent that starts familiar at login and restarts it if
// it crashes. A clean exit (the registration was deleted) isn't restarted.
func install() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	logFile := filepath.Join(home, "Library", "Logs", "familiar.log")
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array><string>%s</string><string>run</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, launchLabel, html.EscapeString(exe), html.EscapeString(logFile), html.EscapeString(logFile))
	if err := os.MkdirAll(filepath.Dir(plistPath()), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(plistPath(), []byte(plist), 0o644); err != nil {
		return err
	}
	domain := "gui/" + strconv.Itoa(os.Getuid())
	_ = exec.Command("launchctl", "bootout", domain, plistPath()).Run()
	if out, err := exec.Command("launchctl", "bootstrap", domain, plistPath()).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl bootstrap: %v: %s", err, out)
	}
	fmt.Println("Familiar now starts at login, and is running. Log:", logFile)
	return nil
}

func uninstall() error {
	domain := "gui/" + strconv.Itoa(os.Getuid())
	_ = exec.Command("launchctl", "bootout", domain, plistPath()).Run()
	if err := os.Remove(plistPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Println("Familiar no longer starts at login, and has stopped.")
	return nil
}

// isLocalDrive is whether a volume is on a drive inside the Mac. Plugged-in
// drives are left out. diskutil is asked once per mount point.
func isLocalDrive(mount string) bool {
	internalMu.Lock()
	defer internalMu.Unlock()
	if v, ok := internalCache[mount]; ok {
		return v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := runQuiet(ctx, "/usr/sbin/diskutil", "info", mount)
	v := err == nil && diskutilInternal(out)
	internalCache[mount] = v
	return v
}

var (
	internalMu    sync.Mutex
	internalCache = map[string]bool{}
)
