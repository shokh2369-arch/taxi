package services

import "strings"

// IsUniqueConstraintErr reports whether err is a SQLite/libSQL unique constraint
// violation.
//
// Matching on the message is deliberate: the libsql client returns opaque errors
// rather than a typed code, and the same code runs against modernc.org/sqlite in
// tests. Both spell it "UNIQUE constraint failed".
func IsUniqueConstraintErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToUpper(err.Error())
	return strings.Contains(msg, "UNIQUE CONSTRAINT FAILED") ||
		strings.Contains(msg, "SQLITE_CONSTRAINT_UNIQUE")
}
