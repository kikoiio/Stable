//go:build darwin

package runtime

type doctorCapability struct {
	Name, Detail string
	OK           bool
}

var doctorCapabilities = []doctorCapability{
	{Name: "sandbox", Detail: "unsupported: Linux bubblewrap isolation is unavailable on macOS", OK: false},
}
