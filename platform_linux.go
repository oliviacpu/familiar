//go:build linux

package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"fyne.io/systray"
	"github.com/godbus/dbus/v5"
)

func attachConsole() {}

func setupLogging() {} // the container's log, or the journal on a desktop

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

//go:embed icons/tray.png
var iconTray []byte // panels don't tint icons, so it's violet like the Windows one

// trayHost says whether there's a desktop session to show the icon in. Panels
// find tray icons over the D-Bus session bus, which the container doesn't have.
func trayHost() bool {
	if _, err := dbus.SessionBus(); err != nil { // systray reuses this connection
		log.Printf("no desktop session, so no icon (%v)", err)
		return false
	}
	return true
}

// prepareTray names the icon before systray publishes it. Panels keep each
// icon's settings (shown or hidden) under its name, and the default one has the
// process ID in it. The panel itself can start after familiar: systray
// registers with it whenever it appears.
func prepareTray(context.Context) { systray.SetTitle("Familiar") }

func hideDockIcon() {}

// What clicking a reading opens: the desktop's system monitor, on the matching
// tab in GNOME's. There's no GPU or battery view to open, so those stay labels.
var (
	openCPU              = systemMonitor("--show-processes-tab")
	openMemory           = systemMonitor("--show-resources-tab")
	openNetwork          = openMemory
	openStorage          = systemMonitor("--show-file-systems-tab")
	openGPU, openBattery opener
)

// systemMonitor opens GNOME's System Monitor on a tab (switching an open one
// too, since it forwards the option to its window), or another desktop's
// monitor as it opens. nil when there isn't one.
func systemMonitor(gnomeTab string) opener {
	if p, err := exec.LookPath("gnome-system-monitor"); err == nil {
		return launcher(p, gnomeTab)
	}
	for _, name := range []string{"plasma-systemmonitor", "mate-system-monitor", "xfce4-taskmanager"} {
		if p, err := exec.LookPath(name); err == nil {
			return launcher(p)
		}
	}
	return nil
}

// tempUnit goes by the locale's name, the way it sets measurement: US means °F.
func tempUnit() string {
	for _, v := range []string{"LC_ALL", "LC_MEASUREMENT", "LANG"} {
		if l := os.Getenv(v); l != "" {
			if strings.Contains(l, "_US") {
				return "F"
			}
			return "C"
		}
	}
	return "C"
}

func printPlatformTemps() {}

func readCPUTemp(context.Context) *float64 { return nil } // hwmon covers it

const unitName = "familiar.service"

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

func unitPath() string {
	dir, err := os.UserConfigDir() // ~/.config, where systemd looks for user units
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "systemd", "user", unitName)
}

// install adds a systemd user service that starts familiar with the desktop
// session and restarts it if it crashes. A clean exit (Quit, or the
// registration was deleted) isn't restarted.
func install() error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return errors.New("familiar install needs systemd; in a container, run familiar as the container's command (see README.md)")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	unit := fmt.Sprintf(`[Unit]
Description=Familiar, reporting this computer's load to Home Assistant
PartOf=graphical-session.target
After=graphical-session.target

[Service]
ExecStart=%s run
Restart=on-failure
RestartSec=10
# Stopping familiar leaves what its menu opened (System Monitor) open.
KillMode=process

[Install]
WantedBy=graphical-session.target
`, systemdQuote(exe))
	if err := os.MkdirAll(filepath.Dir(unitPath()), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(unitPath(), []byte(unit), 0o644); err != nil {
		return err
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", unitName}, {"restart", unitName}} {
		if out, err := exec.Command("systemctl", append([]string{"--user"}, args...)...).CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl --user %s: %v: %s", strings.Join(args, " "), err, out)
		}
	}
	fmt.Println("Familiar now starts with your desktop session, and is running. Log: journalctl --user -u familiar")
	return nil
}

func uninstall() error {
	_ = exec.Command("systemctl", "--user", "disable", "--now", unitName).Run()
	if err := os.Remove(unitPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
	fmt.Println("Familiar no longer starts with your desktop session, and has stopped.")
	return nil
}

// systemdQuote quotes a path for ExecStart, where % and $ are special.
func systemdQuote(s string) string {
	return strconv.Quote(strings.NewReplacer("%", "%%", "$", "$$").Replace(s))
}

// volumeDrive: Linux drive health isn't read, so volumes aren't matched to drives.
func volumeDrive(string) string { return "" }
