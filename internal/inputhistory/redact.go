package inputhistory

import (
	"errors"
	"strings"
)

// minCredentialLength is the shortest secret redact can safely replace;
// shorter values would mangle unrelated text and cannot be identified
// reliably, so writing is refused instead of persisting plaintext.
const minCredentialLength = 8

const redactedPlaceholder = "[credential redacted]"

// redact replaces every configured credential in text, following the same
// strings.ReplaceAll pattern as conversation and execution. It returns an
// error when a credential is too short to redact safely.
func redact(text string, credentials []string) (string, error) {
	for _, credential := range credentials {
		if credential == "" {
			continue
		}
		if len(credential) < minCredentialLength {
			return "", errors.New("credential too short to redact safely")
		}
		text = strings.ReplaceAll(text, credential, redactedPlaceholder)
	}
	return text, nil
}
