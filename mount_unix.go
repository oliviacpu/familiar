//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"syscall"
)

// isMountPoint is whether something is mounted at p: it's on a different
// filesystem than the folder it's in (what the mountpoint command checks).
func isMountPoint(p string) bool {
	if filepath.Clean(p) == "/" {
		return true
	}
	here, err := os.Stat(p)
	if err != nil {
		return false
	}
	parent, err := os.Stat(filepath.Dir(filepath.Clean(p)))
	if err != nil {
		return false
	}
	a, ok1 := here.Sys().(*syscall.Stat_t)
	b, ok2 := parent.Sys().(*syscall.Stat_t)
	return ok1 && ok2 && a.Dev != b.Dev
}
