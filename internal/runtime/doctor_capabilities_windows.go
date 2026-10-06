//go:build windows

package runtime

type doctorCapability struct {
	Name, Detail string
	OK           bool
}

var doctorCapabilities = []doctorCapability{
	{Name: "sandbox", Detail: "unsupported: Linux bubblewrap isolation is unavailable on Windows", OK: false},
}
