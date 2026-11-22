package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

// DriveHealth is what one physical drive says about itself: the NVMe health
// log, or SATA SMART attributes. It's read the way smartctl and CrystalDiskInfo
// read it, so the numbers match theirs.
type DriveHealth struct {
	Device       string   `json:"device"` // "Disk 2" (Windows), "disk0" (Mac)
	Model        string   `json:"model,omitempty"`
	Bus          string   `json:"bus,omitempty"` // NVMe, SATA, USB…
	Status       string   `json:"status"`        // "Healthy", "Not reported", or what's wrong
	Problem      bool     `json:"problem"`
	Temperature  *float64 `json:"temperature,omitempty"`    // °C
	Wear         *float64 `json:"wear,omitempty"`           // % of an SSD's rated life used
	PowerOnHours *float64 `json:"power_on_hours,omitempty"` // hours
}

// healthEvery: drive health changes slowly.
const healthEvery = 10 * time.Minute

// wornOut is the wear at which an SSD counts as a problem.
const wornOut = 90

type healthCache struct {
	at     time.Time
	drives []DriveHealth
}

func (h *healthCache) get(ctx context.Context) []DriveHealth {
	if h.at.IsZero() || time.Since(h.at) >= healthEvery {
		h.drives, h.at = readDriveHealth(ctx), time.Now()
	}
	return h.drives
}

// healthSummary is "Healthy", or each problem drive and what's wrong with it,
// e.g. "Disk 2: 1 pending sector".
func healthSummary(ds []DriveHealth) (string, bool) {
	var bad []string
	for _, d := range ds {
		if d.Problem {
			bad = append(bad, d.Device+": "+d.Status)
		}
	}
	if len(bad) == 0 {
		return "Healthy", false
	}
	return strings.Join(bad, ", "), true
}

// setStatus turns the problems found into the drive's status.
func (d *DriveHealth) setStatus(problems []string) {
	if len(problems) == 0 {
		d.Status, d.Problem = "Healthy", false
		return
	}
	d.Status, d.Problem = strings.Join(problems, ", "), true
}

func plural(n uint64, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// readNVMeLog fills d from the NVMe SMART / health information log (log page
// 2, 512 bytes; NVMe base specification, "SMART / Health Information").
func readNVMeLog(d *DriveHealth, log []byte) {
	if len(log) < 192 {
		d.Status = "Not reported"
		return
	}
	var problems []string
	warn := log[0]
	for _, w := range []struct {
		bit  byte
		what string
	}{{0x01, "spare space low"}, {0x02, "too hot"}, {0x04, "reliability degraded"}, {0x08, "read-only"}, {0x10, "backup power failed"}} {
		if warn&w.bit != 0 {
			problems = append(problems, w.what)
		}
	}
	if k := binary.LittleEndian.Uint16(log[1:3]); k > 273 && k < 273+150 {
		t := math.Round(float64(k) - 273.15)
		d.Temperature = &t
	}
	used := float64(log[5]) // may pass 100 on drives used past their rating
	d.Wear = &used
	if used >= wornOut {
		problems = append(problems, fmt.Sprintf("%.0f%% worn", used))
	}
	poh := float64(binary.LittleEndian.Uint64(log[128:136]))
	d.PowerOnHours = &poh
	if media := binary.LittleEndian.Uint64(log[160:168]); media > 0 {
		problems = append(problems, plural(media, "media error", "media errors"))
	}
	d.setStatus(problems)
}

// readATASmart fills d from SATA SMART READ DATA and READ THRESHOLDS (512
// bytes each: 30 entries of 12 bytes from offset 2).
func readATASmart(d *DriveHealth, data, thresholds []byte) {
	if len(data) < 362 {
		d.Status = "Not reported"
		return
	}
	limit := map[byte]byte{}
	if len(thresholds) >= 362 {
		for i := 0; i < 30; i++ {
			o := 2 + i*12
			limit[thresholds[o]] = thresholds[o+1]
		}
	}
	var problems []string
	var failing []string
	wearID := wearAttribute(d.Model)
	count := func(raw uint64) uint64 { return raw & 0xffffffff } // some drives pack more into the top bytes
	for i := 0; i < 30; i++ {
		o := 2 + i*12
		id := data[o]
		if id == 0 {
			continue
		}
		flags := binary.LittleEndian.Uint16(data[o+1 : o+3])
		current := data[o+3]
		var raw uint64
		for b := 0; b < 6; b++ {
			raw |= uint64(data[o+5+b]) << (8 * b)
		}
		if t := limit[id]; flags&1 != 0 && t != 0 && current <= t {
			failing = append(failing, ataName(id))
		}
		switch id {
		case 5:
			if n := count(raw); n > 0 {
				problems = append(problems, plural(n, "reallocated sector", "reallocated sectors"))
			}
		case 187:
			if n := count(raw); n > 0 {
				problems = append(problems, plural(n, "uncorrectable error", "uncorrectable errors"))
			}
		case 197:
			if n := count(raw); n > 0 {
				problems = append(problems, plural(n, "pending sector", "pending sectors"))
			}
		case 198:
			if n := count(raw); n > 0 {
				problems = append(problems, plural(n, "offline uncorrectable sector", "offline uncorrectable sectors"))
			}
		case 194, 190:
			if t := float64(raw & 0xff); d.Temperature == nil && t > 0 && t < 150 {
				d.Temperature = &t
			}
		case 9:
			h := float64(count(raw))
			d.PowerOnHours = &h
		case wearID:
			if current <= 100 {
				w := float64(100 - current)
				d.Wear = &w
				if w >= wornOut {
					problems = append(problems, fmt.Sprintf("%.0f%% worn", w))
				}
			}
		}
	}
	if len(failing) > 0 {
		problems = append([]string{"failing (" + strings.Join(failing, ", ") + ")"}, problems...)
	}
	d.setStatus(problems)
}

// wearAttribute is the SMART attribute whose normalised value is an SSD's life
// left (100 = new) on that maker's drives, per smartmontools' drive database.
// The same numbers mean other things on other makers' drives, so wear is left
// out for drives not listed (0 matches no attribute).
func wearAttribute(model string) byte {
	m := strings.ToUpper(model)
	switch {
	case strings.Contains(m, "SAMSUNG"):
		return 177 // Wear_Leveling_Count
	case strings.HasPrefix(m, "INTEL"):
		return 233 // Media_Wearout_Indicator
	case strings.HasPrefix(m, "CRUCIAL"), strings.HasPrefix(m, "CT"), strings.HasPrefix(m, "MICRON"):
		return 202 // Percent_Lifetime_Remain
	}
	return 0
}

func ataName(id byte) string {
	if n, ok := map[byte]string{1: "read errors", 3: "spin-up", 5: "reallocated sectors", 7: "seek errors", 10: "spin retries",
		177: "wear", 179: "reserved blocks", 181: "program failures", 182: "erase failures", 196: "reallocations", 231: "life left", 233: "wear"}[id]; ok {
		return n
	}
	return fmt.Sprintf("attribute %d", id)
}

// macOS: `diskutil list physical` names the drives; `diskutil info` gives each
// one's model and, for drives without NVMe SMART, the SMART verdict.

var diskutilDisk = regexp.MustCompile(`(?m)^/dev/(disk\d+) \(([^)]*)\):`)

func parseDiskutilList(out string) []string {
	var ids []string
	for _, m := range diskutilDisk.FindAllStringSubmatch(out, -1) {
		if strings.Contains(m[2], "physical") && strings.Contains(m[2], "internal") {
			ids = append(ids, m[1])
		}
	}
	return ids
}

// diskutilInternal is whether `diskutil info` describes a built-in, fixed drive.
func diskutilInternal(out string) bool {
	return diskutilField(out, "Device Location") == "Internal" && diskutilField(out, "Removable Media") != "Removable"
}

func diskutilField(out, name string) string {
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(k) == name {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func parseDiskutilInfo(id, out string) DriveHealth {
	field := func(name string) string { return diskutilField(out, name) }
	d := DriveHealth{Device: id, Model: field("Device / Media Name"), Bus: field("Protocol")}
	switch s := field("SMART Status"); s {
	case "Verified":
		d.Status = "Healthy"
	case "", "Not Supported":
		d.Status = "Not reported" // USB enclosures usually don't pass SMART through
	default:
		d.Status, d.Problem = s, true // "Failing"
	}
	return d
}
