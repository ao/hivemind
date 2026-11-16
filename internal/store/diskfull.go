package store

import (
	"errors"
	"syscall"

	"modernc.org/sqlite"
)

// sqliteFull is SQLITE_FULL, the primary result code of a write that found
// no space: "database or disk is full". SQLite returns it when write(2)
// fails with ENOSPC, for the database file and its WAL.
const sqliteFull = 13

// IsDiskFull reports whether err is a write that failed because the disk
// that holds the store has no space left. The transaction is rolled back:
// nothing of the change is kept, and reads go on working. The server answers
// such a write with 507 Insufficient Storage.
func IsDiskFull(err error) bool {
	var se *sqlite.Error
	if errors.As(err, &se) && se.Code()&0xff == sqliteFull {
		return true
	}
	return errors.Is(err, syscall.ENOSPC)
}
