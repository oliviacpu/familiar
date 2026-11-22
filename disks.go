package main

import (
	"context"
	"fmt"
	"hash/fnv"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/shirou/gopsutil/v4/disk"
)

// DiskUse is one drive besides the main disk (which stays the "Disk" sensor).
type DiskUse struct {
	ID, Name, Label, Path string
	Used                  float64 // %
	TotalGB, FreeGB       float64
}

// volume is a drive to measure: where it's mounted and what to call it.
type volume struct{ ID, Name, Label, Path string }

// readDisks measures every drive in vols.
func readDisks(ctx context.Context, vols []volume) []DiskUse {
	var out []DiskUse
	for _, v := range vols {
		u, err := disk.UsageWithContext(ctx, v.Path)
		if err != nil || u.Total == 0 {
			continue // unplugged, or not ready (an empty card reader)
		}
		out = append(out, DiskUse{ID: v.ID, Name: v.Name, Label: v.Label, Path: v.Path, Used: u.UsedPercent,
			TotalGB: round(float64(u.Total)/1e9, 1), FreeGB: round(float64(u.Free)/1e9, 1)})
	}
	return out
}

// findVolumes lists this machine's other drives. With explicit paths (the
// a container, which only sees what it's given) it uses those.
func findVolumes(ctx context.Context, goos, main string, explicit []string) []volume {
	if len(explicit) > 0 {
		var parts []disk.PartitionStat
		for _, p := range explicit {
			// The empty folder left when a drive is unmounted would give the
			// numbers of the disk it's on; leave it out so the sensor goes unknown.
			if isMountPoint(p) {
				parts = append(parts, disk.PartitionStat{Mountpoint: p, Device: p})
			}
		}
		return pickVolumes(goos, main, parts, nil)
	}
	parts, _ := disk.PartitionsWithContext(ctx, false) // a warning about one drive still lists the rest
	return pickVolumes(goos, main, parts, isLocalDrive)
}

// pickVolumes leaves out the main disk and names the rest. Discovered mounts
// (keep is set) are filtered to real drives: no system, network, read-only or
// disk-image volumes; keep says whether the OS calls it a local drive.
// Explicit paths (keep is nil) are taken as given.
func pickVolumes(goos, main string, parts []disk.PartitionStat, keep func(string) bool) []volume {
	var out []volume
	seenID, seenDev := map[string]bool{}, map[string]bool{}
	for _, p := range parts {
		if samePath(goos, p.Mountpoint, main) {
			seenDev[p.Device] = true
		}
	}
	for _, p := range parts {
		if samePath(goos, p.Mountpoint, main) {
			continue
		}
		if keep != nil && (!wanted(goos, p) || !keep(p.Mountpoint)) {
			continue
		}
		if seenDev[p.Device] {
			continue // another mount of a drive already counted
		}
		label := volumeLabel(goos, p.Mountpoint)
		id := volumeID(goos, p.Mountpoint)
		if seenID[id] {
			continue // the same path listed twice
		}
		seenID[id], seenDev[p.Device] = true, true
		out = append(out, volume{ID: id, Name: "Disk " + label, Label: label, Path: p.Mountpoint})
	}
	return out
}

// volumeID depends only on the drive's own path, so a drive keeps its sensor
// whatever else is plugged in. It's "disk_" and the drive letter (Windows),
// volume name (Mac; macOS keeps those unique) or mount path (Linux). When the
// slug loses something (punctuation, other scripts, capitals on Linux), a hash
// of the path is added so two such drives can't share an ID.
func volumeID(goos, mount string) string {
	var key string
	var clean *regexp.Regexp // keys that slug maps one to one
	switch goos {
	case "windows":
		key, clean = strings.ToLower(strings.TrimRight(mount, `:\/`)), winKey
	case "darwin":
		key, clean = strings.ToLower(path.Base(mount)), macKey
	default:
		key, clean = strings.Trim(mount, "/"), linuxKey
		if key == "" {
			key = "root"
		}
	}
	id := "disk_" + slug(key)
	if clean.MatchString(key) {
		return id
	}
	h := fnv.New32a()
	h.Write([]byte(mount))
	if id == "disk_" {
		return fmt.Sprintf("disk_%08x", h.Sum32())
	}
	return fmt.Sprintf("%s_%08x", id, h.Sum32())
}

var (
	winKey   = regexp.MustCompile(`^[a-z]$`)
	macKey   = regexp.MustCompile(`^[a-z0-9]+( [a-z0-9]+)*$`)
	linuxKey = regexp.MustCompile(`^[a-z0-9]+(/[a-z0-9]+)*$`)
)

// wanted is whether a discovered mount is a drive someone would call theirs.
func wanted(goos string, p disk.PartitionStat) bool {
	has := func(o string) bool {
		for _, x := range p.Opts {
			if x == o {
				return true
			}
		}
		return false
	}
	switch goos {
	case "darwin":
		// /Volumes holds external drives and opened disk images; images are
		// read-only, and system volumes are hidden (nobrowse) or elsewhere.
		return strings.HasPrefix(p.Mountpoint, "/Volumes/") && has("local") && !has("ro") && !has("nobrowse")
	case "windows":
		return !has("ro") // CDs and DVDs
	default:
		if !strings.HasPrefix(p.Device, "/dev/") || strings.HasPrefix(p.Device, "/dev/loop") {
			return false // virtual filesystems and snaps
		}
		if p.Mountpoint == "/boot" || strings.HasPrefix(p.Mountpoint, "/boot/") {
			return false
		}
		switch p.Fstype {
		case "squashfs", "iso9660":
			return false
		}
		return true
	}
}

// volumeLabel names a drive the way the machine shows it: "D:" on Windows,
// the volume's name on a Mac, the folder it's mounted on in Linux.
func volumeLabel(goos, mount string) string {
	if goos == "windows" {
		return strings.ToUpper(strings.TrimRight(mount, `\/`))
	}
	base := path.Base(filepath.ToSlash(mount))
	if base == "/" || base == "." {
		return "Root"
	}
	if goos != "darwin" {
		r, n := utf8.DecodeRuneInString(base)
		base = string(unicode.ToUpper(r)) + base[n:]
	}
	return base
}

func samePath(goos, a, b string) bool {
	clean := func(p string) string {
		if p != "/" {
			p = strings.TrimRight(p, `\/`)
		}
		if goos == "windows" {
			p = strings.ToUpper(p)
		}
		return p
	}
	return clean(a) == clean(b)
}

var nonWord = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	return strings.Trim(nonWord.ReplaceAllString(strings.ToLower(s), "_"), "_")
}
