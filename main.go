// Command familiar reports a computer's load to Home Assistant.
//
// It sends its readings one of two ways. Through an MQTT broker, where Home
// Assistant finds the computer and its sensors by MQTT discovery; the machine
// needs the broker's address and its own user and password (mqtt.go). Or the
// way the Home Assistant phone app does (the mobile_app integration), to its
// registration's webhook; the machine needs Home Assistant's address and the
// webhook ID, nothing else (ha.go).
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
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
  familiar setup --mqtt-url tcp://homeassistant.local:1883 --name <name> --mqtt-password-stdin
      Report through an MQTT broker instead, with the password from standard input.
      --mqtt-user (default: the device ID, from the name) and --entity-prefix are optional.
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
	mqttURL := fs.String("mqtt-url", "", "MQTT broker, e.g. tcp://homeassistant.local:1883, instead of --url")
	mqttUser := fs.String("mqtt-user", "", "MQTT user (default: the device ID, from --name)")
	passwordStdin := fs.Bool("mqtt-password-stdin", false, "read the MQTT password from standard input")
	prefix := fs.String("entity-prefix", "", "start of this computer's entity IDs, e.g. office_pc (default: from --name)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *mqttURL != "" {
		return setupMQTT(*mqttURL, *mqttUser, *name, *prefix, *passwordStdin)
	}
	if *token == "" {
		*token = os.Getenv("FAMILIAR_TOKEN")
	}
	*token = strings.Trim(strings.TrimSpace(*token), `"'`) // pasted tokens pick up spaces and quotes
	if *url == "" {
		return errors.New("--url is required")
	}
	saved, err := readConfigFile()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	webhookID := *webhook
	if webhookID == "" {
		if *token == "" || *name == "" {
			return errors.New("give either --webhook, or --token and --name to register")
		}
		id, err := registerDevice(ctx, trimURL(*url), *token, *name)
		if err != nil {
			return err
		}
		webhookID = id
		fmt.Printf("Registered %q in Home Assistant.\n", *name)
	}
	cfg := webhookSettings(saved, trimURL(*url), webhookID, *name)
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

// setupMQTT saves the broker and this computer's IDs, then sends one reading
// to check them. The password never comes from a flag, which other users of
// the computer can see: it's read from standard input, or FAMILIAR_MQTT_PASSWORD.
func setupMQTT(broker, user, name, prefix string, passwordStdin bool) error {
	var password *string
	if v, ok := os.LookupEnv("FAMILIAR_MQTT_PASSWORD"); ok {
		password = &v
	}
	if passwordStdin {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("reading the password: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		password = &line
	}
	saved, err := readConfigFile()
	if err != nil {
		return err
	}
	cfg, err := mqttSettings(saved, broker, user, name, prefix, password)
	if err != nil {
		return err
	}
	full := withDefaults(cfg)
	if err := readPasswordFile(&full); err != nil { // the saved password file, when none was given now
		return err
	}
	if err := saveConfig(cfg); err != nil {
		return err
	}
	fmt.Println("Saved settings to", configPath())
	if w := full.userWarning(); w != "" {
		fmt.Println("Warning:", w)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	a := newAgent(full)
	defer a.transport.close()
	n, err := a.tick(ctx, 5*time.Second)
	if err != nil {
		return fmt.Errorf("saved, but sending a reading failed: %w", err)
	}
	fmt.Printf("Sent %d readings to %s as %s (entities %s_…). Next: familiar install\n", n, full.MQTTURL, full.DeviceID, full.EntityPrefix)
	return nil
}

// webhookSettings is the saved settings with a mobile_app setup's. The rest
// stay (auto_update, disks, thresholds…), and MQTT is turned off, since
// mqtt_url is what picks the transport.
func webhookSettings(saved Config, url, webhookID, name string) Config {
	cfg := saved
	cfg.URL, cfg.WebhookID, cfg.MQTTURL = url, webhookID, ""
	if name != "" {
		cfg.Name = name
	}
	return cfg
}

// mqttSettings is the saved settings with an MQTT setup's. The rest stay,
// the webhook's too (mqtt_url picks the transport, and removing it goes
// back). So do IDs saved before, so renaming the computer doesn't move its
// topics or entities. password is nil when none was given: the saved one stays.
func mqttSettings(saved Config, broker, user, name, prefix string, password *string) (Config, error) {
	cfg := saved
	cfg.MQTTURL = strings.TrimSpace(broker)
	if name != "" {
		cfg.Name = name
	}
	if cfg.Name == "" {
		return Config{}, errors.New("--name is required with --mqtt-url")
	}
	if cfg.DeviceID == "" {
		cfg.DeviceID = nameSlug(cfg.Name)
	}
	if prefix != "" {
		cfg.EntityPrefix = prefix
	}
	if cfg.EntityPrefix = slug(cfg.EntityPrefix); cfg.EntityPrefix == "" {
		cfg.EntityPrefix = nameSlug(cfg.Name)
	}
	if user != "" {
		cfg.MQTTUser = user
	}
	if cfg.MQTTUser == "" {
		cfg.MQTTUser = cfg.DeviceID
	}
	if password != nil {
		cfg.MQTTPassword, cfg.MQTTPasswordFile = secret(*password), "" // a file would win over it
	}
	if err := withDefaults(cfg).validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
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
	log.Printf("familiar %s reporting to %s every %s", version, cfg.destination(), cfg.interval())
	if w := cfg.userWarning(); w != "" {
		log.Print(w)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runReporter(ctx, newAgent(cfg)) // with a menu bar or tray icon, or headless without a desktop
}

// loop reports until ctx ends or Home Assistant deletes the registration.
// after, if given, runs after each report (the menu uses it to refresh).
func loop(ctx context.Context, a *agent, after func()) error {
	defer a.transport.close() // MQTT: offline, before the menu's Quit ends the program
	a.collector.Read(ctx)     // starting point for CPU and network rates
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
