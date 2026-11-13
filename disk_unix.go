//go:build !windows

package main

import "syscall"

// freeShare is the share of the disk holding path that is free for the site (0 to 1).
func freeShare(path string) (float64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return float64(st.Bavail) / float64(st.Blocks), nil
}
