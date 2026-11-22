// Command familiar reports a computer's load to Home Assistant.
//
// It registers itself the way the Home Assistant phone app does (the mobile_app
// integration) and then sends its readings to that registration's webhook. The
// machine needs Home Assistant's address and the webhook ID, nothing else.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/sensors"
)

var version = "dev"

func main() {
	attachConsole()
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "setup":
		err = cmdSetup(os.Args[2:])
	case "run":
		err = cmdRun()
	case "once":
		err = cmdOnce()
	case "temps":
		err = cmdTemps()
	case "drives":
		err = cmdDrives(os.Args[2:])
	case "install":
		err = install()
	case "uninstall":
		err = uninstall()
	case "version":
		fmt.Println("familiar", version)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "familiar:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `familiar reports this computer's load to Home Assistant.

  familiar setup --url http://homeassistant.local:8123 --webhook <id>
      Save the address and webhook, then send one reading to check it works.
  familiar setup --url http://homeassistant.local:8123 --token <token> --name <name>
      Register this computer in Home Assistant first. The token is used once and not saved.
  familiar once        Print one reading here. Sends nothing.
  familiar drives      Print each drive's health (temperature, wear, errors).
  familiar temps       List every temperature sensor, and which ones count as CPU and GPU.
  familiar run         Keep reporting (what the login item runs).
  familiar install     Start familiar at login (macOS, Windows and Linux desktops).
  familiar uninstall   Stop starting it at login.
  familiar version
`)
}

func cmdSetup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	url := fs.String("url", "", "Home Assistant address, e.g. http://homeassistant.local:8123")
	webhook := fs.String("webhook", "", "webhook ID of an existing registration")
	token := fs.String("token", "", "long-lived access token, used once to register")
	name := fs.String("name", "", "name for this computer (registers it under this name with --token)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *token == "" {
		*token = os.Getenv("FAMILIAR_TOKEN")
	}
	*token = strings.Trim(strings.TrimSpace(*token), `"'`) // pasted tokens pick up spaces and quotes
	if *url == "" {
		return errors.New("--url is required")
	}
	cfg := Config{URL: trimURL(*url), WebhookID: *webhook, Name: *name}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if cfg.WebhookID == "" {
		if *token == "" || *name == "" {
			return errors.New("give either --webhook, or --token and --name to register")
		}
		id, err := registerDevice(ctx, cfg.URL, *token, *name)
		if err != nil {
			return err
		}
		cfg.WebhookID = id
		fmt.Printf("Registered %q in Home Assistant.\n", *name)
	}
	if err := saveConfig(cfg); err != nil {
		return err
	}
	cfg = withDefaults(cfg)
	fmt.Println("Saved settings to", configPath())

	a := newAgent(cfg)
	n, err := a.tick(ctx, 5*time.Second)
	if err != nil {
		return fmt.Errorf("saved, but sending a reading failed: %w", err)
	}
	fmt.Printf("Sent %d readings to %s. Next: familiar install\n", n, cfg.URL)
	return nil
}

func cmdOnce() error {
	cfg, _ := loadConfig()
	cfg = withDefaults(cfg)
	c := newCollector(cfg)
	c.Read(context.Background())
	time.Sleep(5 * time.Second)
	r := c.Read(context.Background())
	out, _ := json.MarshalIndent(buildSensors(r, nil, cfg), "", "  ")
	fmt.Println(string(out))
	return nil
}

// cmdDrives prints each physical drive's health, as Home Assistant gets it.
func cmdDrives(args []string) error {
	ds := collectDriveHealth(context.Background())
	if len(args) > 0 && args[0] == "--json" {
		out, _ := json.Marshal(ds)
		fmt.Println(string(out))
		return nil
	}
	if len(ds) == 0 {
		fmt.Println("No drive health here (on Windows, run as administrator).")
		return nil
	}
	num := func(p *float64, unit string) string {
		if p == nil {
			return "—"
		}
		return fmt.Sprintf("%.0f%s", *p, unit)
	}
	for _, d := range ds {
		fmt.Printf("%-8s %-32s %-5s %6s %5s %8s  %s\n", d.Device, d.Model, d.Bus, num(d.Temperature, " °C"), num(d.Wear, "%"), num(d.PowerOnHours, " h"), d.Status)
	}
	status, _ := healthSummary(ds)
	fmt.Println("Drive health:", status)
	return nil
}

// cmdTemps lists the temperature sensors this machine has, for working out
// which ones are the CPU and GPU on a new chip.
func cmdTemps() error {
	ctx := context.Background()
	if info, err := host.InfoWithContext(ctx); err == nil {
		fmt.Printf("%s %s %s (%s)\n", info.Platform, info.PlatformVersion, info.KernelArch, chipName(ctx))
	}
	temps, err := sensors.TemperaturesWithContext(ctx)
	if len(temps) == 0 {
		// Keep going: PawnIO, the SMC and the GPU driver below don't need these sensors.
		fmt.Println("No generic temperature sensors found.", err)
	}
	cpu, gpu := pickTemps(runtime.GOOS, temps)
	sort.Slice(temps, func(i, j int) bool { return temps[i].SensorKey < temps[j].SensorKey })
	for _, t := range temps {
		mark := ""
		if cpu != nil && t.Temperature == *cpu {
			mark += "  ← CPU"
		}
		if gpu != nil && t.Temperature == *gpu {
			mark += "  ← GPU"
		}
		fmt.Printf("  %-40s %6.1f °C%s\n", t.SensorKey, t.Temperature, mark)
	}
	printPlatformTemps()
	if _, driverTemp := readGPU(ctx); driverTemp != nil {
		fmt.Printf("GPU temperature reported: %.1f °C (from the GPU driver or SMC, used instead of the sensors above).\n", *driverTemp)
	} else if gpu == nil {
		fmt.Println("No sensor counted as the GPU.")
	}
	return nil
}

func sortedKeys(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func chipName(ctx context.Context) string {
	if infos, err := cpu.InfoWithContext(ctx); err == nil && len(infos) > 0 && infos[0].ModelName != "" {
		return infos[0].ModelName
	}
	return "unknown chip"
}

func cmdRun() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cfg = withDefaults(cfg)
	if err := cfg.validate(); err != nil {
		return err
	}
	setupLogging()
	cleanupOldSelf()
	log.Printf("familiar %s reporting to %s every %s", version, cfg.URL, cfg.interval())

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runReporter(ctx, newAgent(cfg)) // with a menu bar or tray icon, or headless without a desktop
}

// loop reports until ctx ends or Home Assistant deletes the registration.
// after, if given, runs after each report (the menu uses it to refresh).
func loop(ctx context.Context, a *agent, after func()) error {
	a.collector.Read(ctx) // starting point for CPU and network rates
	first := time.NewTimer(5 * time.Second)
	ticker := time.NewTicker(a.cfg.interval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Print("stopping")
			return nil
		case <-first.C:
		case <-ticker.C:
		}
		tctx, cancel := context.WithTimeout(ctx, a.cfg.interval())
		_, err := a.tick(tctx, 0)
		cancel()
		if after != nil {
			after()
		}
		if a.updates != nil {
			a.updates.maybeUpdate(ctx) // restarts into the new build if there is one
		}
		if errors.Is(err, errGone) {
			// Exit cleanly so the login item doesn't keep restarting it.
			log.Print("Home Assistant no longer knows this computer; run familiar setup again")
			return nil
		}
		if err != nil {
			log.Print(err)
		}
	}
}
