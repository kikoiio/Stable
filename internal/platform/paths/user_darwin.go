//go:build darwin

package paths

func defaultConfigHome() (string, error) { return userHomeJoin("Library", "Application Support") }
func defaultStateHome() (string, error)  { return userHomeJoin("Library", "Application Support") }
