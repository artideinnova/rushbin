//go:build !darwin

package main

import "os"

// noCache: elsewhere a copy read back may come from the system's memory, so it does not count as checked.
// ponytail: Windows has FILE_FLAG_NO_BUFFERING, with aligned reads; until then Duplicati reads the copy again.
func noCache(*os.File) bool { return false }
