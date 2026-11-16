//go:build !darwin && !windows

package main

import (
	"io"
	"os"
)

// Elsewhere a copy read back may come from the system's memory, so it does not count as checked.
func noCache(*os.File) bool { return false }

func openFromDrive(p string) (io.ReadCloser, bool, error) {
	f, err := os.Open(p)
	return f, false, err
}
