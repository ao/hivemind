//go:build unix

package main

import (
	"io/fs"
	"syscall"
)

// fileOwner returns the user ID that owns the file fi describes.
func fileOwner(fi fs.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
