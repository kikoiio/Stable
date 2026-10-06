package ipc

import (
	"crypto/sha1"
	"encoding/hex"
)

// PipeName maps a state socket path to a stable Windows named-pipe name.
func PipeName(path string) string {
	sum := sha1.Sum([]byte(path))
	return `\\.\pipe\stable\` + hex.EncodeToString(sum[:])[:16]
}

// PipeSDDL builds a current-user-only named-pipe DACL for a SID. It is kept
// platform-neutral so the descriptor grammar can be tested on Linux.
func PipeSDDL(sid string) string { return "D:P(A;;GA;;;" + sid + ")" }

// CurrentUserPipeSDDL returns a valid owner-only descriptor for callers that
// do not need to resolve a token. The Windows listener substitutes the actual
// token SID through PipeSDDL.
func CurrentUserPipeSDDL() string { return PipeSDDL("OW") }
