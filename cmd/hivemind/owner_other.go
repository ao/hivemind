//go:build !unix

package main

import "io/fs"

// fileOwner reports no owner where files have no user ID, so the CLI does
// not name another user's cluster there.
func fileOwner(fs.FileInfo) (int, bool) { return 0, false }
