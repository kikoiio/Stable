package redact

import "testing"

func TestRedactReplacesEveryOccurrence(t *testing.T) {
	got, err := Redact("a sk-test-key-123 b sk-test-key-123", []string{"sk-test-key-123"})
	if err != nil {
		t.Fatal(err)
	}
	want := "a " + Placeholder + " b " + Placeholder
	if got != want {
		t.Fatalf("redact: %q want %q", got, want)
	}
}

func TestRedactMultipleCredentials(t *testing.T) {
	got, err := Redact("x first-secret y second-secret", []string{"first-secret", "second-secret"})
	if err != nil {
		t.Fatal(err)
	}
	want := "x " + Placeholder + " y " + Placeholder
	if got != want {
		t.Fatalf("redact: %q want %q", got, want)
	}
}

func TestRedactSkipsEmptyCredentials(t *testing.T) {
	got, err := Redact("unchanged", []string{"", ""})
	if err != nil {
		t.Fatal(err)
	}
	if got != "unchanged" {
		t.Fatalf("empty credential changed text: %q", got)
	}
}

func TestRedactRefusesShortCredential(t *testing.T) {
	// Refusal is unconditional: a caller cannot trust the returned text
	// while any configured credential is unsafe to replace.
	if _, err := Redact("no secret here", []string{"short"}); err == nil {
		t.Fatal("short credential accepted")
	}
}

func TestRedactRefusesShortCredentialAmongLongOnes(t *testing.T) {
	if _, err := Redact("x long-enough-secret y", []string{"long-enough-secret", "tiny"}); err == nil {
		t.Fatal("short credential accepted among long ones")
	}
}
