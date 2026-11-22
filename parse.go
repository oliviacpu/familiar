package main

// Parsers for what each OS reports. They take plain text or plain values so
// that every platform's parsing is tested on any machine.

import (
	"bufio"
	"encoding/csv"
	"encoding/xml"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/shirou/gopsutil/v4/sensors"
)

// pickTemps chooses the CPU and GPU temperatures (°C) from gopsutil's sensors.
func pickTemps(goos string, stats []sensors.TemperatureStat) (cpu, gpu *float64) {
	var cpuKeys, cpuFallback, gpuKeys []string
	switch goos {
	case "linux":
		// Intel package, AMD Tctl/Tdie, Raspberry Pi; then any Intel core.
		cpuKeys = []string{"coretemp_package", "x86_pkg_temp", "k10temp_tctl", "k10temp_tdie", "zenpower", "cpu_thermal"}
		cpuFallback = []string{"coretemp"}
		gpuKeys = []string{"amdgpu", "radeon", "nouveau"}
	case "darwin":
		// Apple silicon names the CPU clusters "pACC/eACC MTR Temp Sensor N"; Intel Macs use SMC keys.
		// GPU sensor names vary by chip, so any sensor with "GPU" in its name counts.
		cpuKeys = []string{"ACC MTR Temp", "TC0D", "TC0P", "TC0E", "TC0F"}
		cpuFallback = []string{"tdie"}
		gpuKeys = []string{"GPU", "TG0D", "TG0P"}
	case "windows":
		// None: Windows' ACPI thermal zones often report a fixed made-up value.
		// The real CPU temperature comes from PawnIO (cputemp_windows.go).
	}
	cpu = maxMatching(stats, cpuKeys)
	if cpu == nil {
		cpu = maxMatching(stats, cpuFallback)
	}
	return cpu, maxMatching(stats, gpuKeys)
}

func maxMatching(stats []sensors.TemperatureStat, keys []string) *float64 {
	var best *float64
	for _, s := range stats {
		t := s.Temperature
		if t <= 0 || t >= 150 || math.IsNaN(t) {
			continue // unset sensors read 0; nothing real is 150 °C
		}
		for _, k := range keys {
			if strings.Contains(strings.ToLower(s.SensorKey), strings.ToLower(k)) {
				if best == nil || t > *best {
					v := t
					best = &v
				}
				break
			}
		}
	}
	return best
}

// parseNvidiaSMI reads `nvidia-smi --query-gpu=utilization.gpu,temperature.gpu
// --format=csv,noheader,nounits`. With several GPUs it reports the busiest and hottest.
func parseNvidiaSMI(out string) (util, temp *float64) {
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		parts := strings.Split(sc.Text(), ",")
		if len(parts) < 2 {
			continue
		}
		if u, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64); err == nil && (util == nil || u > *util) {
			util = &u
		}
		if t, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64); err == nil && (temp == nil || t > *temp) {
			temp = &t
		}
	}
	return util, temp
}

// parseTypeperf reads `typeperf "\GPU Engine(*engtype_3D)\Utilization Percentage" -sc 2`,
// a CSV with one column per engine. The 3D engines' last sample, added up, is the
// GPU load Task Manager shows.
func parseTypeperf(out string) *float64 {
	r := csv.NewReader(strings.NewReader(out))
	r.FieldsPerRecord = -1
	rows, _ := r.ReadAll()
	var last []string
	for _, row := range rows {
		if len(row) < 2 || strings.HasPrefix(row[0], "(PDH-CSV") {
			continue // the header
		}
		if _, err := strconv.ParseFloat(strings.TrimSpace(row[1]), 64); err == nil {
			last = row
		}
	}
	if last == nil {
		return nil
	}
	sum := 0.0
	for _, v := range last[1:] {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			sum += f
		}
	}
	sum = math.Min(sum, 100)
	return &sum
}

var ioregUtil = regexp.MustCompile(`"Device Utilization %"\s*=\s*(\d+)`)

// parseIoregGPU reads `ioreg -r -d 1 -w 0 -c IOAccelerator` on a Mac.
func parseIoregGPU(out string) *float64 {
	var best *float64
	for _, m := range ioregUtil.FindAllStringSubmatch(out, -1) {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil && (best == nil || v > *best) {
			best = &v
		}
	}
	return best
}

var pmsetBatt = regexp.MustCompile(`(\d+)%;\s*([^;]+);`)

// parsePmset reads `pmset -g batt`. Charging means plugged in and not yet full.
func parsePmset(out string) (percent *float64, charging *bool) {
	m := pmsetBatt.FindStringSubmatch(out)
	if m == nil {
		return nil, nil
	}
	p, _ := strconv.ParseFloat(m[1], 64)
	status := strings.TrimSpace(m[2])
	c := status == "charging" || status == "finishing charge"
	return &p, &c
}

// countInterface says whether an interface's traffic is the machine's own
// network use, as opposed to loopback, VPNs, containers and VMs.
func countInterface(name string) bool {
	n := strings.ToLower(name)
	if n == "lo" || n == "lo0" {
		return false
	}
	for _, p := range []string{
		"docker", "br-", "veth", "virbr", "vnet", "tailscale", "utun", "tun", "tap", "wg", "zt",
		"awdl", "llw", "bridge", "gif", "stf", "anpi", "ap1", "vethernet", "loopback", "isatap",
		"teredo", "vmnet", "vmware", "virtualbox", "bluetooth", "local area connection*",
	} {
		if strings.HasPrefix(n, p) {
			return false
		}
	}
	return true
}

// fourCC turns an SMC key like "Tg0G" into the number the SMC uses, and back.
func fourCC(s string) uint32 {
	var k uint32
	for i := 0; i < 4 && i < len(s); i++ {
		k = k<<8 | uint32(s[i])
	}
	return k
}

func fourCCString(k uint32) string {
	return string([]byte{byte(k >> 24), byte(k >> 16), byte(k >> 8), byte(k)})
}

// decodeSMCTemp reads an SMC temperature. Apple silicon stores them as a
// little-endian float ("flt "); Intel Macs as signed 8.8 fixed point ("sp78").
func decodeSMCTemp(dataType uint32, b []byte) (float64, bool) {
	switch fourCCString(dataType) {
	case "flt ":
		if len(b) >= 4 {
			return float64(math.Float32frombits(uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24)), true
		}
	case "sp78":
		if len(b) >= 2 {
			return float64(int16(uint16(b[0])<<8|uint16(b[1]))) / 256, true
		}
	}
	return 0, false
}

// averagePlausible averages readings that look like real temperatures. Some
// SMC keys for absent parts read 0 or a fixed junk value.
func averagePlausible(temps []float64) *float64 {
	sum, n := 0.0, 0
	for _, t := range temps {
		if t > 10 && t < 125 && !math.IsNaN(t) {
			sum += t
			n++
		}
	}
	if n == 0 {
		return nil
	}
	avg := sum / float64(n)
	return &avg
}

// intelPackageTemp is the CPU package temperature on Intel: TjMax, from
// IA32_TEMPERATURE_TARGET bits 23:16, minus how far below it the package is,
// from IA32_PACKAGE_THERM_STATUS bits 22:16.
func intelPackageTemp(tempTarget, pkgTherm uint64) (float64, bool) {
	tjMax := float64((tempTarget >> 16) & 0xFF)
	if tjMax < 50 || tjMax > 125 {
		tjMax = 100 // what LibreHardwareMonitor assumes when TjMax isn't reported
	}
	t := tjMax - float64((pkgTherm>>16)&0x7F)
	return t, t > 0 && t < 125
}

// amdTctl is the CPU temperature on AMD Zen, from SMN 0x59800
// (THM_TCON_CUR_TMP), the way Linux's k10temp and LibreHardwareMonitor read it.
func amdTctl(raw uint32, cpuName string) float64 {
	t := float64(raw>>21) * 0.125
	if raw&0x80000 != 0 || raw&0x30000 == 0x30000 { // the 49 °C range offset
		t -= 49
	}
	switch { // Tctl runs ahead of the real die temperature on these
	case strings.Contains(cpuName, "1600X"), strings.Contains(cpuName, "1700X"), strings.Contains(cpuName, "1800X"):
		t -= 20
	case strings.Contains(cpuName, "Threadripper 19"), strings.Contains(cpuName, "Threadripper 29"):
		t -= 27
	case strings.Contains(cpuName, "2700X"):
		t -= 10
	}
	return t
}

// pawnioInput is a PawnIO execute request: the function name in 32 bytes,
// zero-padded, then the arguments as little-endian 64-bit numbers.
func pawnioInput(fn string, args ...int64) []byte {
	b := make([]byte, 32+8*len(args))
	copy(b[:31], fn)
	for i, a := range args {
		v := uint64(a)
		for j := 0; j < 8; j++ {
			b[32+8*i+j] = byte(v >> (8 * j))
		}
	}
	return b
}

// pawnioVersionOK says whether an installed PawnIO is new enough (2.0 or
// later, which the official modules need).
func pawnioVersionOK(v string) bool {
	major, err := strconv.Atoi(strings.SplitN(strings.TrimSpace(v), ".", 2)[0])
	return err == nil && major >= 2
}

// taskXML defines the scheduled task that starts familiar at sign-in with
// administrator rights (for PawnIO), in the signed-in user's session so the
// tray icon shows. No time limit: the default would stop it after 3 days.
func taskXML(sid, exe string) string {
	esc := func(s string) string {
		var b strings.Builder
		_ = xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	return `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><Description>Familiar reports this computer's load to Home Assistant.</Description></RegistrationInfo>
  <Triggers><LogonTrigger><Enabled>true</Enabled><UserId>` + esc(sid) + `</UserId></LogonTrigger></Triggers>
  <Principals><Principal id="Author"><UserId>` + esc(sid) + `</UserId><LogonType>InteractiveToken</LogonType><RunLevel>HighestAvailable</RunLevel></Principal></Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>false</StartWhenAvailable>
    <IdleSettings><StopOnIdleEnd>false</StopOnIdleEnd><RestartOnIdle>false</RestartOnIdle></IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>7</Priority>
  </Settings>
  <Actions Context="Author"><Exec><Command>` + esc(exe) + `</Command><Arguments>run</Arguments></Exec></Actions>
</Task>
`
}

// utf16File encodes text as UTF-16LE with a byte-order mark, which is what
// schtasks expects for task XML.
func utf16File(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2+2*len(u))
	b[0], b[1] = 0xFF, 0xFE
	for i, c := range u {
		b[2+2*i], b[3+2*i] = byte(c), byte(c>>8)
	}
	return b
}
