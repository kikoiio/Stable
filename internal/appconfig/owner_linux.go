//go:build linux

package appconfig

import (
	"os"
	"syscall"
)

func ownedByCurrentUser(st os.FileInfo) (bool, error) {
	sys, ok := st.Sys().(*syscall.Stat_t)
	return ok && sys.Uid == uint32(os.Getuid()), nil
}
