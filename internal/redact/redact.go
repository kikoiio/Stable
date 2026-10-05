// Package redact removes configured credentials from text before it is
// persisted or sent onward.
package redact

import (
	"errors"
	"strings"
)

// minCredentialLength is the shortest secret Redact can safely replace;
// shorter values cannot be identified reliably and would mangle unrelated
// text, so Redact refuses them instead of replacing.
const minCredentialLength = 8

// Placeholder replaces every occurrence of a redacted credential.
const Placeholder = "[credential redacted]"

// Redact replaces every configured credential in text with Placeholder.
// Empty credentials are skipped. A credential shorter than
// minCredentialLength bytes is refused with an error and no replacement is
// attempted, because such a value cannot be identified reliably in text.
func Redact(text string, credentials []string) (string, error) {
	for _, credential := range credentials {
		if credential == "" {
			continue
		}
		if len(credential) < minCredentialLength {
			return "", errors.New("credential too short to redact safely")
		}
		text = strings.ReplaceAll(text, credential, Placeholder)
	}
	return text, nil
}
