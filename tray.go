//go:build darwin || windows || linux

package main

import (
	"context"
	_ "embed"
	"log"
	"os"
	"os/exec"
	"strings"

	"fyne.io/systray"
)

//go:embed icons/menubar.png
var iconMenuBar []byte // black on clear; macOS tints it for light and dark menu bars

// runReporter puts an icon in the macOS menu bar, the Windows notification area
// or a Linux desktop's panel, with the latest readings and Quit in its menu, and
// reports in the background. There's no window, Dock icon or taskbar button.
// Without a desktop (in a container, say) it just reports.
//
// Reporting doesn't wait for the icon. When Windows refuses one (at sign-in,
// before Explorer is ready), systray logs it and never calls onReady, and the
// readings must still go out.
func runReporter(ctx context.Context, a *agent) error {
	if !trayHost() {
		return loop(ctx, a, nil)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ready := make(chan struct{}) // closed once the icon and its menu exist
	var items *menuItems
	done := make(chan error, 1)
	go func() {
		done <- loop(ctx, a, func() {
			select {
			case <-ready:
				items.show(a.menu())
			default:
			}
		})
		cancel()
		select {
		case <-ready: // onReady's goroutine below ends systray.Run
		default:
			os.Exit(0) // there's no icon, so systray.Run would never return
		}
	}()

	prepareTray(ctx)
	systray.Run(func() {
		hideDockIcon()
		systray.SetTemplateIcon(iconMenuBar, iconTray)
		systray.SetTooltip("Familiar")
		items = newMenuItems(ctx)
		systray.AddSeparator()
		quit := systray.AddMenuItem("Quit Familiar", "Stop reporting until you next sign in")
		items.show(a.menu())
		close(ready)
		go func() {
			select {
			case <-quit.ClickedCh:
			case <-ctx.Done():
			}
			cancel()
			systray.Quit()
		}()
	}, func() {})
	cancel()
	return <-done
}

type menuItems struct {
	title, problem, activity, cpu, memory, disk, drives, gpu, network, battery *systray.MenuItem
}

// opener is what clicking a reading does. Each platform sets openCPU and the
// rest; a nil one leaves its reading disabled.
type opener func()

// launcher opens an app or settings page with a command.
func launcher(argv ...string) opener { return func() { launch(argv) } }

// newMenuItems lays out the menu. A reading with somewhere to look closer
// opens it when clicked. Everything else is disabled, so it reads as a label
// rather than a button.
func newMenuItems(ctx context.Context) *menuItems {
	label := func() *systray.MenuItem { it := systray.AddMenuItem("", ""); it.Disable(); return it }
	opens := func(open opener) *systray.MenuItem {
		if open == nil {
			return label()
		}
		it := systray.AddMenuItem("", "")
		go func() {
			for {
				select {
				case <-it.ClickedCh:
					open()
				case <-ctx.Done():
					return
				}
			}
		}()
		return it
	}
	m := &menuItems{title: label(), problem: label(), activity: opens(openCPU)}
	systray.AddSeparator()
	m.cpu, m.memory, m.disk, m.drives = opens(openCPU), opens(openMemory), opens(openStorage), opens(openStorage)
	m.gpu, m.network, m.battery = opens(openGPU), opens(openNetwork), opens(openBattery)
	return m
}

// launch starts an app or settings page without waiting for it to close.
func launch(argv []string) {
	cmd := exec.Command(argv[0], argv[1:]...)
	if err := cmd.Start(); err != nil {
		log.Printf("opening %s: %v", strings.Join(argv, " "), err)
		return
	}
	go func() { _ = cmd.Wait() }()
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
	set(m.title, t.Title)
	set(m.problem, t.Problem)
	set(m.activity, t.Activity)
	set(m.cpu, t.CPU)
	set(m.memory, t.Memory)
	set(m.disk, t.Disk)
	set(m.drives, t.Drives)
	set(m.gpu, t.GPU)
	set(m.network, t.Network)
	set(m.battery, t.Battery)
}
