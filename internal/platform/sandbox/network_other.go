//go:build !linux

package sandbox

import (
	"context"
	"errors"
)

func enableLoopback(context.Context) error {
	return errors.New("isolated network proxy is supported only on Linux")
}
