package inputhistory

import redactpkg "stable/internal/redact"

const redactedPlaceholder = redactpkg.Placeholder

// redact replaces every configured credential in text before it is
// persisted. It returns an error when a credential is too short to redact
// safely: the write is refused instead of persisting plaintext.
func redact(text string, credentials []string) (string, error) {
	return redactpkg.Redact(text, credentials)
}
