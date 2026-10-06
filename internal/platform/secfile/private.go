package secfile

import (
	"os"
	"runtime"
)

// currentUserSDDL builds a DACL that grants full access only to the given SID.
// Format matches Windows SECURITY_DESCRIPTOR SDDL: D:P(A;;FA;;;<sid>).
func currentUserSDDL(sid string) string {
	return "D:P(A;;FA;;;" + sid + ")"
}

// currentUserPipeSDDL builds a pipe DACL granting generic-all to the SID.
func currentUserPipeSDDL(sid string) string {
	return "D:P(A;;GA;;;" + sid + ")"
}

// permDeniesOthers reports whether perm has no other/group bits set.
func permDeniesOthers(perm os.FileMode) bool {
	return perm&0077 == 0
}

// fixHint returns a short remediation hint for private-file failures.
// kind is "file" or "dir".
func fixHint(kind string) string {
	if runtime.GOOS == "windows" {
		if kind == "dir" {
			return "icacls /inheritance:r /grant:r %USERNAME%:F on the config directory"
		}
		return "icacls /inheritance:r /grant:r %USERNAME%:F on the config file"
	}
	if kind == "dir" {
		return "chmod 700 and confirm ownership of the config directory"
	}
	return "chmod 600 and confirm ownership of the config file"
}
