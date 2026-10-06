//go:build linux

package runtime

type doctorCapability struct {
	Name, Detail string
	OK           bool
}

// Linux keeps the historical doctor output stable; sandbox availability is
// exercised by the runtime and sandbox contract tests.
var doctorCapabilities = []doctorCapability{}
