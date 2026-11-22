//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func attachConsole() {}

func setupLogging() {} // the container's log

func runQuiet(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err
}

// readGPU asks NVIDIA's tool, which the NVIDIA container runtime puts in the
// container. Without it there's no GPU reading.
func readGPU(ctx context.Context) (util, temp *float64) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := runQuiet(ctx, "nvidia-smi", "--query-gpu=utilization.gpu,temperature.gpu", "--format=csv,noheader,nounits")
	if err != nil {
		return nil, nil
	}
	return parseNvidiaSMI(out)
}

func readBattery(context.Context) (*float64, *bool) { return nil, nil }

// runReporter shows a panel icon on a Linux desktop (a session bus and a
// display), and otherwise reports headless, as in a container.
func runReporter(ctx context.Context, a *agent) error {
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") != "" && (os.Getenv("WAYLAND_DISPLAY") != "" || os.Getenv("DISPLAY") != "") {
		return runTray(ctx, a)
	}
	return loop(ctx, a, nil)
}

func hideDockIcon() {}

func tempUnit() string { return "C" }

func printPlatformTemps() {}

func readCPUTemp(context.Context) *float64 { return nil } // hwmon covers it

// On a Linux desktop, familiar starts at sign-in from an XDG autostart entry,
// the standard place for a panel icon. In a container it's the container's
// command instead (see README.md).
func autostartPath() string {
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "autostart", "familiar.desktop")
}

func install() error {
	exe, err := selfPath()
	if err != nil {
		return err
	}
	entry := "[Desktop Entry]\nType=Application\nName=Familiar\nComment=Reports this computer's load to Home Assistant\n" +
		"Exec=\"" + exe + "\" run\nTerminal=false\nX-GNOME-Autostart-enabled=true\n"
	if err := os.MkdirAll(filepath.Dir(autostartPath()), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(autostartPath(), []byte(entry), 0o644); err != nil {
		return err
	}
	stopOthers()
	cmd := exec.Command(exe, "run")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	fmt.Println("Familiar now starts when you sign in, and is running. Entry:", autostartPath())
	return nil
}

func uninstall() error {
	if err := os.Remove(autostartPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	stopOthers()
	fmt.Println("Familiar no longer starts when you sign in, and has stopped.")
	return nil
}

// stopOthers ends other familiar processes run from this binary.
func stopOthers() {
	exe, err := selfPath()
	if err != nil {
		return
	}
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		if p, err := os.Readlink(filepath.Join("/proc", e.Name(), "exe")); err == nil && p == exe {
			syscall.Kill(pid, syscall.SIGTERM)
		}
	}
}

// isLocalDrive leaves out removable and USB drives; wanted() already left out
// network mounts. mount is found again in /proc/self/mounts for its device.
func isLocalDrive(mount string) bool {
	dev := mountDevice(mount)
	if dev == "" {
		return true
	}
	return !removableBlock(dev)
}

// Linux drive health isn't read yet; smartctl on the host covers it.
func readDriveHealth(context.Context) []DriveHealth { return nil }

func collectDriveHealth(context.Context) []DriveHealth { return nil }

func mountDevice(mount string) string {
	data, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[1] == mount && strings.HasPrefix(f[0], "/dev/") {
			return f[0]
		}
	}
	return ""
}

// removableBlock: the kernel's removable flag, or a drive on USB. A partition
// (sdb1) is checked through its disk (sdb).
func removableBlock(dev string) bool {
	real, err := filepath.EvalSymlinks(dev)
	if err != nil {
		return false
	}
	sys, err := filepath.EvalSymlinks(filepath.Join("/sys/class/block", filepath.Base(real)))
	if err != nil {
		return false
	}
	if strings.Contains(sys, "/usb") {
		return true
	}
	for _, dir := range []string{sys, filepath.Dir(sys)} {
		if b, err := os.ReadFile(filepath.Join(dir, "removable")); err == nil {
			return strings.TrimSpace(string(b)) == "1"
		}
	}
	return false
}
