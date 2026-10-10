//go:build !linux

package sandbox

func BoundedWorkspaceVolume(string, ...string) error { return ErrUnavailable }
