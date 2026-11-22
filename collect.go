package main

import (
	"context"
	"runtime"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/sensors"
)

// Reading is one set of measurements. Pointers are nil when this machine
// can't measure that thing.
type Reading struct {
	CPU, Memory, Disk float64
	DiskDrive         string        // the physical drive the main disk is on, "" if unknown
	Disks             []DiskUse     // the other drives
	Health            []DriveHealth // nil where the OS can't say
	CPUTemp           *float64
	GPU, GPUTemp      *float64
	NetDown, NetUp    *float64 // Mbit/s, since the previous reading
	Boot              time.Time
	Battery           *float64
	Charging          *bool
}

type collector struct {
	disk               string
	disks              []string // other drives, when they're listed instead of found
	lastRecv, lastSent uint64
	lastNet            time.Time
	health             healthCache
	drives             driveMap
}

func newCollector(cfg Config) *collector { return &collector{disk: cfg.DiskPath, disks: cfg.Disks} }

// Read measures everything. CPU use and network rates are averages since the
// previous Read, so the first one only sets the starting point for those.
func (c *collector) Read(ctx context.Context) Reading {
	var r Reading
	if p, err := cpu.PercentWithContext(ctx, 0, false); err == nil && len(p) > 0 {
		r.CPU = p[0]
	}
	if m, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		r.Memory = m.UsedPercent
	}
	if d, err := disk.UsageWithContext(ctx, c.disk); err == nil {
		r.Disk = d.UsedPercent
	}
	// Looked for every time, so a drive plugged in shows up and one taken out goes unknown.
	r.Disks = readDisks(ctx, findVolumes(ctx, runtime.GOOS, c.disk, c.disks))
	r.Health = c.health.get(ctx)
	// Which physical drive each volume is on, so its sensor can carry that
	// drive's health: a failing SSD flags every letter on it.
	if len(r.Health) > 0 {
		r.DiskDrive = c.drives.of(c.disk)
		for i := range r.Disks {
			r.Disks[i].Drive = c.drives.of(r.Disks[i].Path)
		}
	}
	if bt, err := host.BootTimeWithContext(ctx); err == nil && bt > 0 {
		r.Boot = time.Unix(int64(bt), 0)
	}
	// gopsutil can return readings together with an error about other sensors.
	if temps, _ := sensors.TemperaturesWithContext(ctx); len(temps) > 0 {
		r.CPUTemp, r.GPUTemp = pickTemps(runtime.GOOS, temps)
	}
	if r.CPUTemp == nil {
		r.CPUTemp = readCPUTemp(ctx) // Windows, through PawnIO
	}
	util, temp := readGPU(ctx)
	r.GPU = util
	if temp != nil {
		r.GPUTemp = temp // the driver's own reading beats a nearby sensor
	}
	r.Battery, r.Charging = readBattery(ctx)

	if counters, err := net.IOCountersWithContext(ctx, true); err == nil {
		var recv, sent uint64
		for _, n := range counters {
			if countInterface(n.Name) {
				recv += n.BytesRecv
				sent += n.BytesSent
			}
		}
		now := time.Now()
		if secs := now.Sub(c.lastNet).Seconds(); !c.lastNet.IsZero() && secs > 0 && recv >= c.lastRecv && sent >= c.lastSent {
			down := round(float64(recv-c.lastRecv)*8/1e6/secs, 3)
			up := round(float64(sent-c.lastSent)*8/1e6/secs, 3)
			r.NetDown, r.NetUp = &down, &up
		}
		c.lastRecv, c.lastSent, c.lastNet = recv, sent, now
	}
	return r
}
