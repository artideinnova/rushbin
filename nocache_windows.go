package main

import (
	"io"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// noCache: on Windows what is written goes through the system's memory; the copy is read back past it
// (openFromDrive), once flushed to the drive.
func noCache(*os.File) bool { return false }

// openFromDrive opens p to be read from the drive itself (FILE_FLAG_NO_BUFFERING): reads then go in whole
// sectors, into memory aligned to them, so they go through a buffer of 1 MiB aligned to 4096 bytes (any sector
// size up to that); at the end of the file the drive gives back only the bytes there are. direct says it is.
func openFromDrive(p string) (io.ReadCloser, bool, error) {
	name, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return nil, false, err
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_NO_BUFFERING|windows.FILE_FLAG_SEQUENTIAL_SCAN, 0)
	if err != nil { // not allowed there: read as usual, and the copy does not count as checked
		f, err := os.Open(p)
		return f, false, err
	}
	raw := make([]byte, 1<<20+4096)
	off := int((4096 - uintptr(unsafe.Pointer(&raw[0]))%4096) % 4096)
	return &sectors{h: h, buf: raw[off : off+1<<20]}, true, nil
}

type sectors struct {
	h         windows.Handle
	buf, rest []byte
}

func (s *sectors) Read(b []byte) (int, error) {
	if len(s.rest) == 0 {
		var n uint32
		if err := windows.ReadFile(s.h, s.buf, &n, nil); err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, io.EOF
		}
		s.rest = s.buf[:n]
	}
	n := copy(b, s.rest)
	s.rest = s.rest[n:]
	return n, nil
}

func (s *sectors) Close() error { return windows.CloseHandle(s.h) }
