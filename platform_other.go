//go:build !windows

package main

import (
	"errors"
	"os/exec"
)

func driveRoots() []string { return nil }

func identifyWindows(vol string) (Device, error) {
	return Device{Mount: vol}, errors.New("not Windows")
}

func lowPriority(*exec.Cmd) {}

func revealFile(path string) error { return exec.Command("/usr/bin/open", "-R", path).Run() }

func openBrowser(string) error { return nil } // the Mac app shows the page in its own window

// watchPower: the Mac app sends the conditions on stdin (readConditions).
func (a *App) watchPower() {}
