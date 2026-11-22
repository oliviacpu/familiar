//go:build darwin || windows || linux

package main

import (
	"context"
	_ "embed"
	"runtime"

	"fyne.io/systray"
)

//go:embed icons/menubar.png
var iconMenuBar []byte // black on clear; macOS tints it for light and dark menu bars

//go:embed icons/tray.ico
var iconTray []byte // Windows notification area

//go:embed icons/tray-linux.png
var iconLinux []byte // Linux panels (StatusNotifierItem): grey, readable on light and dark

// runTray puts an icon in the macOS menu bar, the Windows notification area or
// a Linux desktop's panel, with the latest readings and Quit in its menu, and
// reports in the background. There's no window, Dock icon or taskbar button.
func runTray(ctx context.Context, a *agent) error {
	done := make(chan error, 1)
	systray.Run(func() {
		hideDockIcon()
		if runtime.GOOS == "linux" {
			systray.SetIcon(iconLinux)
		} else {
			systray.SetTemplateIcon(iconMenuBar, iconTray)
		}
		systray.SetTooltip("Familiar")
		items := newMenuItems()
		systray.AddSeparator()
		quit := systray.AddMenuItem("Quit Familiar", "Stop reporting until you next sign in")
		items.show(a.menu())

		ctx, cancel := context.WithCancel(ctx)
		go func() {
			select {
			case <-quit.ClickedCh:
			case <-ctx.Done():
			}
			cancel()
			systray.Quit()
		}()
		go func() {
			done <- loop(ctx, a, func() { items.show(a.menu()) })
			cancel()
		}()
	}, func() {})
	select {
	case err := <-done:
		return err
	default:
		return nil
	}
}

type menuItems struct {
	name, activity, cpu, memory, disk, drives, gpu, network, battery, status *systray.MenuItem
}

func newMenuItems() *menuItems {
	header := func() *systray.MenuItem { it := systray.AddMenuItem("", ""); it.Disable(); return it }
	line := func() *systray.MenuItem { return systray.AddMenuItem("", "") } // clicking a reading just closes the menu
	m := &menuItems{name: header(), activity: line()}
	systray.AddSeparator()
	m.cpu, m.memory, m.disk, m.drives, m.gpu, m.network, m.battery = line(), line(), line(), line(), line(), line(), line()
	systray.AddSeparator()
	m.status = header()
	return m
}

func (m *menuItems) show(t menuText) {
	set := func(it *systray.MenuItem, s string) {
		if s == "" {
			it.Hide()
			return
		}
		it.SetTitle(s)
		it.Show()
	}
	set(m.name, t.Name)
	set(m.activity, t.Activity)
	set(m.cpu, t.CPU)
	set(m.memory, t.Memory)
	set(m.disk, t.Disk)
	set(m.drives, t.Drives)
	set(m.gpu, t.GPU)
	set(m.network, t.Network)
	set(m.battery, t.Battery)
	set(m.status, t.Status)
}
