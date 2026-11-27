package main

import (
	"context"
	"math"
	"sort"
	"sync"
	"time"
)

// activity is whether the machine is doing real work: its typical (median) CPU
// or GPU load over the last few readings is above a threshold. The median
// means one short spike doesn't count but most of the window does. Memory
// isn't used: apps keep memory even when the machine is idle.
type activity struct {
	Busy   bool
	CPUAvg float64  // median CPU load over the window
	GPUAvg *float64 // median GPU load, when there's a GPU reading
}

type busyTracker struct{ cpu, gpu []float64 }

func (b *busyTracker) add(cpu float64, gpu *float64, cfg Config) activity {
	b.cpu = lastN(append(b.cpu, cpu), cfg.BusySamples)
	if gpu != nil {
		b.gpu = lastN(append(b.gpu, *gpu), cfg.BusySamples)
	} else if len(b.gpu) > 0 {
		// No GPU reading this time: age out the oldest sample, so a GPU that
		// stops reporting can't hold Activity at Busy with an old median.
		b.gpu = b.gpu[1:]
	}
	a := activity{CPUAvg: round(median(b.cpu), 1)}
	a.Busy = a.CPUAvg >= cfg.BusyCPU
	if len(b.gpu) > 0 {
		g := round(median(b.gpu), 1)
		a.GPUAvg = &g
		a.Busy = a.Busy || g >= cfg.BusyGPU
	}
	return a
}

// buildSensors turns a reading into webhook sensors, leaving out anything this
// machine can't measure.
func buildSensors(r Reading, act *activity, cfg Config) []Sensor {
	pct := func(id, name, icon string, v float64) Sensor {
		return Sensor{UniqueID: id, Type: "sensor", Name: name, Icon: icon, State: round(v, 1), Unit: "%", StateClass: "measurement"}
	}
	temp := func(id, name string, v float64) Sensor {
		return Sensor{UniqueID: id, Type: "sensor", Name: name, State: round(v, 1), Unit: "°C", DeviceClass: "temperature", StateClass: "measurement"}
	}
	rate := func(id, name, icon string, v float64) Sensor {
		return Sensor{UniqueID: id, Type: "sensor", Name: name, Icon: icon, State: v, Unit: "Mbit/s", DeviceClass: "data_rate", StateClass: "measurement"}
	}
	out := []Sensor{
		pct("cpu", "CPU", "mdi:cpu-64-bit", r.CPU),
		pct("memory", "Memory", "mdi:memory", r.Memory),
		pct("disk", "Disk", "mdi:harddisk", r.Disk),
	}
	if a := driveAttributes(r.Health, r.DiskDrive); a != nil {
		out[2].Attributes = a
	}
	for _, d := range r.Disks {
		s := pct(d.ID, d.Name, "mdi:harddisk", d.Used)
		s.Attributes = map[string]any{"path": d.Path, "size_gb": d.TotalGB, "free_gb": d.FreeGB}
		for k, v := range driveAttributes(r.Health, d.Drive) {
			s.Attributes[k] = v
		}
		out = append(out, s)
	}
	if len(r.Health) > 0 {
		status, problem := healthSummary(r.Health)
		out = append(out, Sensor{UniqueID: "drive_problem", Type: "binary_sensor", Name: "Drive problem", State: problem,
			DeviceClass: "problem", Icon: "mdi:harddisk", Attributes: map[string]any{"status": status, "disks": r.Health}})
	}
	if r.CPUTemp != nil {
		out = append(out, temp("cpu_temperature", "CPU temperature", *r.CPUTemp))
	}
	if r.GPU != nil {
		out = append(out, pct("gpu", "GPU", "mdi:expansion-card", *r.GPU))
	}
	if r.GPUTemp != nil {
		out = append(out, temp("gpu_temperature", "GPU temperature", *r.GPUTemp))
	}
	if r.NetDown != nil && r.NetUp != nil {
		out = append(out, rate("network_down", "Network down", "mdi:download-network", *r.NetDown),
			rate("network_up", "Network up", "mdi:upload-network", *r.NetUp))
	}
	if !r.Boot.IsZero() {
		out = append(out, Sensor{UniqueID: "last_boot", Type: "sensor", Name: "Last boot", Icon: "mdi:restart",
			State: r.Boot.UTC().Format(time.RFC3339), DeviceClass: "timestamp", EntityCategory: "diagnostic"})
	}
	if r.Battery != nil {
		out = append(out, Sensor{UniqueID: "battery", Type: "sensor", Name: "Battery", State: round(*r.Battery, 0),
			Unit: "%", DeviceClass: "battery", StateClass: "measurement"})
	}
	if r.Charging != nil {
		out = append(out, Sensor{UniqueID: "charging", Type: "binary_sensor", Name: "Charging", State: *r.Charging, DeviceClass: "battery_charging"})
	}
	if act != nil {
		s := Sensor{UniqueID: "activity", Type: "sensor", Name: "Activity", State: "Idle", Icon: "mdi:sleep",
			Attributes: map[string]any{"cpu_typical": act.CPUAvg, "busy_cpu_percent": cfg.BusyCPU}}
		if act.Busy {
			s.State, s.Icon = "Busy", "mdi:run-fast"
		}
		if act.GPUAvg != nil {
			s.Attributes["gpu_typical"] = *act.GPUAvg
			s.Attributes["busy_gpu_percent"] = cfg.BusyGPU
		}
		out = append(out, s)
	}
	return out
}

// transport carries readings to Home Assistant: the mobile_app webhook
// (mobileApp, ha.go).
type transport interface {
	// send delivers one reading. A sensor with only its UniqueID and Type and
	// a nil State has gone away, and shows as unknown.
	send(ctx context.Context, sensors []Sensor) error
	// knows is whether Home Assistant has been told about a sensor, so one
	// that goes away can be cleared.
	knows(id string) bool
	// close says goodbye, where there's a connection to end.
	close()
}

type agent struct {
	cfg       Config
	collector *collector
	transport transport
	updates   *updater // nil when auto-update is off
	busy      busyTracker
	types     map[string]string // sensors sent before, so a missing one can be cleared

	// The last reading and how sending it went, for the menu. mu guards them:
	// the menu first reads them while the loop is already reporting.
	mu      sync.Mutex
	last    Reading
	lastAct activity
	sentAt  time.Time
	lastErr error
}

func newAgent(cfg Config) *agent {
	a := &agent{cfg: cfg, collector: newCollector(cfg), transport: newMobileApp(cfg.URL, cfg.WebhookID), types: map[string]string{}}
	if cfg.autoUpdate() {
		a.updates = newUpdater()
	}
	return a
}

// tick takes a reading and sends it. With prime > 0 it first takes a starting
// reading and waits, for when nothing has been read yet (setup).
func (a *agent) tick(ctx context.Context, prime time.Duration) (int, error) {
	if prime > 0 {
		a.collector.Read(ctx)
		select {
		case <-time.After(prime):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	return a.report(ctx, a.collector.Read(ctx))
}

// report sends a reading, and keeps it and how sending went for the menu.
func (a *agent) report(ctx context.Context, r Reading) (int, error) {
	act := a.busy.add(r.CPU, r.GPU, a.cfg)
	out := buildSensors(r, &act, a.cfg)
	present := map[string]bool{}
	for _, s := range out {
		present[s.UniqueID] = true
		a.types[s.UniqueID] = s.Type
	}
	// A reading that has gone away (say the GPU tool stopped answering) shows as unknown.
	for id, typ := range a.types {
		if !present[id] && a.transport.knows(id) {
			out = append(out, Sensor{UniqueID: id, Type: typ, State: nil})
		}
	}
	err := a.transport.send(ctx, out)
	a.mu.Lock()
	a.last, a.lastAct, a.lastErr = r, act, err
	if err == nil {
		a.sentAt = time.Now()
	}
	a.mu.Unlock()
	return len(out), err
}

func (a *agent) menu() menuText {
	unit := tempUnit()
	a.mu.Lock()
	defer a.mu.Unlock()
	return buildMenu(a.cfg.Name, a.last, a.lastAct, a.sentAt, a.lastErr, unit)
}

func median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

func lastN(xs []float64, n int) []float64 {
	if len(xs) > n {
		return xs[len(xs)-n:]
	}
	return xs
}

func round(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}
