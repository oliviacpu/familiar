//go:build windows

package main

// `familiar install` on Windows, in one step: it asks for administrator rights
// once (a UAC prompt), installs the PawnIO driver if it's missing (for the CPU
// temperature), and starts familiar at sign-in with highest privileges through
// a scheduled task. Running familiar without the prompt each time needs the
// task; a Run key entry can't start an elevated program.

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// The official PawnIO 2.2.0 installer, signed by namazso. It isn't kept in
// git: the build downloads it from github.com/namazso/PawnIO.Setup and checks
// this hash (see .github/workflows/familiar.yml and README.md).
//
//go:embed pawnio/PawnIO_setup.exe
var pawnSetup []byte

const pawnSetupSHA256 = "1f519a22e47187f70a1379a48ca604981c4fcf694f4e65b734aaa74a9fba3032"

const (
	runKey   = `Software\Microsoft\Windows\CurrentVersion\Run`
	taskName = "Familiar"
)

func install() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if !isElevated() {
		if err := runElevated(exe, "install"); err != nil {
			return err
		}
		fmt.Println("Approve the Windows prompt. Familiar finishes setting itself up as administrator and shows a message when it's done.")
		return nil
	}
	summary, err := installElevated(exe)
	if err != nil {
		message("Familiar couldn't finish installing:\n\n"+err.Error(), true)
		return err
	}
	message(summary, false)
	fmt.Println(summary)
	return nil
}

func installElevated(exe string) (string, error) {
	pawn, err := ensurePawnIO()
	if err != nil {
		return "", err
	}
	tok := windows.GetCurrentProcessToken()
	user, err := tok.GetTokenUser()
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "familiar-task")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	xmlPath := filepath.Join(dir, "familiar.xml")
	if err := os.WriteFile(xmlPath, utf16File(taskXML(user.User.Sid.String(), exe)), 0o600); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if out, err := runQuietCombined(ctx, "schtasks", "/Create", "/TN", taskName, "/XML", xmlPath, "/F"); err != nil {
		return "", fmt.Errorf("creating the scheduled task: %v: %s", err, strings.TrimSpace(out))
	}
	removeRunKey() // the older, non-administrator way of starting at sign-in
	stopOthers()
	if out, err := runQuietCombined(ctx, "schtasks", "/Run", "/TN", taskName); err != nil {
		return "", fmt.Errorf("starting the scheduled task: %v: %s", err, strings.TrimSpace(out))
	}
	temp := "CPU temperature: " + cpuTempReason
	if t := readCPUTemp(ctx); t != nil {
		temp = fmt.Sprintf("CPU temperature now: %.0f °C", *t)
	}
	return fmt.Sprintf("Familiar is installed and running.\n\nIt starts when you sign in, with administrator rights, with no prompt.\n%s\n%s", pawn, temp), nil
}

// ensurePawnIO installs the bundled PawnIO if it's missing or older than 2.0.
func ensurePawnIO() (string, error) {
	if v := pawnioVersion(); pawnioVersionOK(v) {
		return "PawnIO " + v + " was already installed.", nil
	}
	sum := sha256.Sum256(pawnSetup)
	if hex.EncodeToString(sum[:]) != pawnSetupSHA256 {
		return "", errors.New("the bundled PawnIO installer doesn't match its pinned hash")
	}
	dir, err := os.MkdirTemp("", "familiar-pawnio")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	setup := filepath.Join(dir, "PawnIO_setup.exe")
	if err := os.WriteFile(setup, pawnSetup, 0o700); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, setup, "-install", "-silent")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	runErr := cmd.Run()
	if v := pawnioVersion(); pawnioVersionOK(v) {
		return "Installed PawnIO " + v + ".", nil
	}
	return "", fmt.Errorf("PawnIO's installer finished, but PawnIO isn't installed (%v)", runErr)
}

func uninstall() error {
	removeRunKey()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if _, err := runQuietCombined(ctx, "schtasks", "/Query", "/TN", taskName); err != nil {
		stopOthers()
		fmt.Println("Familiar no longer starts at sign-in, and has stopped.")
		return nil
	}
	if !isElevated() {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		if err := runElevated(exe, "uninstall"); err != nil {
			return err
		}
		fmt.Println("Approve the Windows prompt to remove Familiar's scheduled task.")
		return nil
	}
	if out, err := runQuietCombined(ctx, "schtasks", "/Delete", "/TN", taskName, "/F"); err != nil {
		return fmt.Errorf("deleting the scheduled task: %v: %s", err, strings.TrimSpace(out))
	}
	stopOthers()
	msg := "Familiar no longer starts at sign-in, and has stopped.\n\nPawnIO stays installed; remove it in Settings › Apps if nothing else uses it."
	message(msg, false)
	fmt.Println(msg)
	return nil
}

// runElevated starts this program again with administrator rights (UAC).
func runElevated(exe, args string) error {
	verb, _ := windows.UTF16PtrFromString("runas")
	file, _ := windows.UTF16PtrFromString(exe)
	params, _ := windows.UTF16PtrFromString(args)
	if err := windows.ShellExecute(0, verb, file, params, nil, windows.SW_HIDE); err != nil {
		if errors.Is(err, windows.ERROR_CANCELLED) {
			return errors.New("the Windows prompt was declined; nothing changed")
		}
		return fmt.Errorf("asking Windows for administrator rights: %w", err)
	}
	return nil
}

func removeRunKey() {
	if k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE); err == nil {
		_ = k.DeleteValue("Familiar")
		k.Close()
	}
}

// message shows a dialog: the administrator step has no console to print to.
func message(text string, isError bool) {
	t, _ := windows.UTF16PtrFromString(text)
	c, _ := windows.UTF16PtrFromString("Familiar")
	var style uint32 = windows.MB_OK | windows.MB_ICONINFORMATION
	if isError {
		style = windows.MB_OK | windows.MB_ICONERROR
	}
	_, _ = windows.MessageBox(0, t, c, style)
}

func runQuietCombined(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
	out, err := cmd.CombinedOutput()
	return string(out), err
}
