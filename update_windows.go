//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

func canReplaceSelf() bool {
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	f, err := os.OpenFile(exe+".check", os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(exe + ".check")
	return true
}

// replaceSelfAndRestart: Windows won't overwrite a running .exe but lets it be
// renamed, so the old one moves aside, the new one takes its name and starts,
// and this one exits. The old file is removed at the next start.
func replaceSelfAndRestart(bin []byte) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	old := exe + ".old"
	os.Remove(old)
	if err := os.WriteFile(exe+".new", bin, 0o755); err != nil {
		return err
	}
	if err := os.Rename(exe, old); err != nil {
		os.Remove(exe + ".new")
		return err
	}
	if err := os.Rename(exe+".new", exe); err != nil {
		os.Rename(old, exe) // put the old one back
		return err
	}
	cmd := exec.Command(exe, "run")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x00000008 | 0x00000200} // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
	if err := cmd.Start(); err != nil {
		os.Rename(exe, exe+".new")
		os.Rename(old, exe)
		return err
	}
	os.Exit(0)
	return nil
}

func cleanupOldSelf() {
	if exe, err := os.Executable(); err == nil {
		os.Remove(exe + ".old")
	}
}
