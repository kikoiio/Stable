package lock

// Guard holds an exclusive runtime lock until Release.
type Guard interface {
	Release() error
}

// TryAcquire creates path (mode 0600) and takes a non-blocking exclusive lock.
func TryAcquire(path string) (Guard, error) {
	return tryAcquire(path)
}
