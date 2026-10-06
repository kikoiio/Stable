//go:build linux

package paths

func defaultConfigHome() (string, error) { return userHomeJoin(".config") }
func defaultStateHome() (string, error)  { return userHomeJoin(".local", "state") }
