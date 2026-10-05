//go:build !linux

package lock

import "errors"

func tryAcquire(string) (Guard, error) {
	return nil, errors.New("lock: exclusive flock is only supported on Linux")
}
