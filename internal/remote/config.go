// Package remote contains the listener and browser-access implementation.
package remote

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

const (
	// DefaultListenAddress keeps the remote service reachable only from the
	// local machine unless the user explicitly opts into a LAN listener.
	DefaultListenAddress = "127.0.0.1:8765"
)

// Config describes the address and optional TLS credentials for the remote
// HTTP/WebSocket listener.
type Config struct {
	ListenAddress string
	CertFile      string
	KeyFile       string
}

// DefaultConfig returns the safe default remote listener configuration.
func DefaultConfig() Config {
	return Config{ListenAddress: DefaultListenAddress}
}

// EffectiveListenAddress returns the configured address, or the loopback
// default when the field is empty.
func (c Config) EffectiveListenAddress() string {
	if strings.TrimSpace(c.ListenAddress) == "" {
		return DefaultListenAddress
	}
	return c.ListenAddress
}

// IsLoopback reports whether the configured IP literal is loopback. Hostnames
// are rejected because their resolution could change between validation and
// binding.
func (c Config) IsLoopback() (bool, error) {
	host, _, err := net.SplitHostPort(c.EffectiveListenAddress())
	if err != nil {
		return false, fmt.Errorf("invalid remote listen address: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false, errors.New("remote listen address must use an IP literal")
	}
	return ip.IsLoopback(), nil
}

// Validate checks the bind address and ensures every non-loopback listener is
// protected by a loadable TLS certificate and private key. Callers must only
// bind after validation succeeds.
func (c Config) Validate() error {
	address := c.EffectiveListenAddress()
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid remote listen address: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return errors.New("remote listen address must use an IP literal")
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 0 || portNumber > 65535 {
		return errors.New("remote listen address must include a valid TCP port")
	}

	hasCert := strings.TrimSpace(c.CertFile) != ""
	hasKey := strings.TrimSpace(c.KeyFile) != ""
	if hasCert != hasKey {
		return errors.New("remote TLS requires both a certificate file and a private key file")
	}
	if !ip.IsLoopback() && !hasCert {
		return errors.New("non-loopback remote listeners require TLS certificate and private key files")
	}
	if hasCert {
		if _, err := tls.LoadX509KeyPair(c.CertFile, c.KeyFile); err != nil {
			return fmt.Errorf("invalid remote TLS certificate or key: %w", err)
		}
	}
	return nil
}

// LoadTLSCertificate loads the already validated TLS identity for use by the
// HTTPS listener. It reports an error when TLS credentials are not configured.
func (c Config) LoadTLSCertificate() (tls.Certificate, error) {
	if strings.TrimSpace(c.CertFile) == "" || strings.TrimSpace(c.KeyFile) == "" {
		return tls.Certificate{}, errors.New("remote TLS certificate and private key are not configured")
	}
	return tls.LoadX509KeyPair(c.CertFile, c.KeyFile)
}
