package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// noCache keeps what f writes and reads out of the system's memory (F_NOCACHE): read back, f comes from the drive.
func noCache(f *os.File) bool {
	_, err := unix.FcntlInt(f.Fd(), unix.F_NOCACHE, 1)
	return err == nil
}
