package main

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// noCache keeps what f writes and reads out of the system's memory (F_NOCACHE).
func noCache(f *os.File) bool {
	_, err := unix.FcntlInt(f.Fd(), unix.F_NOCACHE, 1)
	return err == nil
}

// openFromDrive opens p to be read from the drive itself, not from the system's memory; direct says it is.
func openFromDrive(p string) (r io.ReadCloser, direct bool, err error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, false, err
	}
	return f, noCache(f), nil
}
