//go:build !linux

package appconfig

import (
	"errors"
	"os"
)

func ownedByCurrentUser(os.FileInfo) (bool, error) {
	return false, errors.New("appconfig: owner checks are only supported on Linux")
}
