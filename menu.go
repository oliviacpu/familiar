package main

import (
	"fmt"
	"strings"
	"time"
)

// menuText is what the menu bar or tray menu shows. An empty line is hidden.
type menuText struct {
	Name, Activity, CPU, Memory, Disk, Drives, GPU, Network, Battery, Status string
}

// buildMenu describes the last reading for the menu. unit is "F" or "C".
func buildMenu(name string, r Reading, act activity, sentAt time.Time, sendErr error, unit string) menuText {
	m := menuText{
		Name:     name,
		Activity: "Idle",
		CPU:      fmt.Sprintf("CPU %.0f%%", r.CPU) + tempSuffix(r.CPUTemp, unit),
		Memory:   fmt.Sprintf("Memory %.0f%%", r.Memory),
		Disk:     fmt.Sprintf("Disk %.0f%%", r.Disk),
	}
	for _, d := range r.Disks {
		m.Disk += fmt.Sprintf(" · %s %.0f%%", d.Label, d.Used)
	}
	if status, problem := healthSummary(r.Health); problem {
		m.Drives = "Drive problem: " + status // only when there is one
	}
	if act.Busy {
		m.Activity = "Busy"
	}
	if r.GPU != nil {
		m.GPU = fmt.Sprintf("GPU %.0f%%", *r.GPU) + tempSuffix(r.GPUTemp, unit)
	} else if r.GPUTemp != nil {
		m.GPU = "GPU" + tempSuffix(r.GPUTemp, unit)
	}
	if r.NetDown != nil && r.NetUp != nil {
		m.Network = fmt.Sprintf("Network ↓ %s ↑ %s Mbit/s", mbit(*r.NetDown), mbit(*r.NetUp))
	}
	if r.Battery != nil {
		m.Battery = fmt.Sprintf("Battery %.0f%%", *r.Battery)
		if r.Charging != nil && *r.Charging {
			m.Battery += ", charging"
		}
	}
	switch {
	case sendErr != nil:
		m.Status = "Not sent: " + shorten(sendErr.Error(), 60)
	case !sentAt.IsZero():
		m.Status = "Sent to Home Assistant at " + sentAt.Format("3:04 PM")
	default:
		m.Status = "Starting…"
	}
	return m
}

func tempSuffix(c *float64, unit string) string {
	if c == nil {
		return ""
	}
	if unit == "F" {
		return fmt.Sprintf(", %.0f °F", *c*9/5+32)
	}
	return fmt.Sprintf(", %.0f °C", *c)
}

// mbit shows small rates with a decimal and large ones without.
func mbit(v float64) string {
	if v < 10 {
		return fmt.Sprintf("%.1f", v)
	}
	return fmt.Sprintf("%.0f", v)
}

func shorten(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
