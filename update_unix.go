//go:build darwin || linux

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"syscall"
)

// canReplaceSelf: the binary's folder is writable (a container's isn't).
func canReplaceSelf() bool {
	exe, err := selfPath()
	if err != nil {
		return false
	}
	f, err := os.CreateTemp(filepath.Dir(exe), ".familiar-check-*")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(f.Name())
	return true
}

func selfPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// replaceSelfAndRestart swaps the binary in one rename, then starts it: on a
// Mac by exiting, so launchd starts the new one (the LaunchAgent restarts it
// after an unsuccessful exit); on Linux by running it in place.
func replaceSelfAndRestart(bin []byte) error {
	exe, err := selfPath()
	if err != nil {
		return err
	}
	tmp := exe + ".new"
	if err := os.WriteFile(tmp, bin, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Remove(tmp)
		return err
	}
	if runtime.GOOS == "darwin" {
		os.Exit(exitRestart)
	}
	return syscall.Exec(exe, os.Args, os.Environ())
}

func cleanupOldSelf() {}
