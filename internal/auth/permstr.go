package auth

import "fmt"

// PermFromText parses the SQL string form ("read"|"write"|"admin") of a Perm.
func PermFromText(s string) (Perm, error) {
	switch s {
	case "read":
		return PermRead, nil
	case "write":
		return PermWrite, nil
	case "admin":
		return PermAdmin, nil
	default:
		return PermNone, fmt.Errorf("auth: unknown perm %q", s)
	}
}

// PermToText returns the SQL string form of a Perm. Empty string for PermNone.
func PermToText(p Perm) string {
	switch p {
	case PermRead:
		return "read"
	case PermWrite:
		return "write"
	case PermAdmin:
		return "admin"
	default:
		return ""
	}
}
