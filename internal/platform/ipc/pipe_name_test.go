package ipc

import "testing"

func TestPipeName(t *testing.T) {
	a, b := PipeName("/tmp/stable/control.sock"), PipeName("/tmp/stable/control.sock")
	if a != b || len(a) < len(`\\.\pipe\stable\`) {
		t.Fatalf("pipe name is not stable: %q %q", a, b)
	}
}

func TestCurrentUserPipeSDDL(t *testing.T) {
	if got, want := PipeSDDL("S-1-5-18"), "D:P(A;;GA;;;S-1-5-18)"; got != want {
		t.Fatalf("PipeSDDL = %q, want %q", got, want)
	}
	if got := CurrentUserPipeSDDL(); got != "D:P(A;;GA;;;OW)" {
		t.Fatalf("CurrentUserPipeSDDL = %q", got)
	}
}
