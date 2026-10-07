package remote

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRemoteConfigDefaults(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.ListenAddress != DefaultListenAddress {
		t.Fatalf("default listen address = %q, want %q", cfg.ListenAddress, DefaultListenAddress)
	}
	if got := (Config{}).EffectiveListenAddress(); got != DefaultListenAddress {
		t.Fatalf("empty config listen address = %q, want %q", got, DefaultListenAddress)
	}
	if err := (Config{}).Validate(); err != nil {
		t.Fatalf("validate default config: %v", err)
	}
}

func TestRemoteConfigLoopbackNeedsNoTLS(t *testing.T) {
	for _, address := range []string{"127.0.0.1:8765", "[::1]:8765"} {
		t.Run(address, func(t *testing.T) {
			cfg := Config{ListenAddress: address}
			loopback, err := cfg.IsLoopback()
			if err != nil {
				t.Fatalf("IsLoopback: %v", err)
			}
			if !loopback {
				t.Fatal("expected loopback address")
			}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("validate loopback config: %v", err)
			}
		})
	}
}

func TestRemoteConfigNonLoopbackRequiresCertificateAndKey(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "both missing",
			cfg:  Config{ListenAddress: "0.0.0.0:8765"},
			want: "non-loopback",
		},
		{
			name: "key missing",
			cfg:  Config{ListenAddress: "192.168.1.10:8765", CertFile: "cert.pem"},
			want: "both a certificate",
		},
		{
			name: "certificate missing",
			cfg:  Config{ListenAddress: "192.168.1.10:8765", KeyFile: "key.pem"},
			want: "both a certificate",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error = %v, want substring %q", err, tt.want)
			}
		})
	}
}

func TestRemoteConfigLoadsSelfSignedTLSCertificate(t *testing.T) {
	certPath, keyPath := writeSelfSignedPair(t)
	cfg := Config{ListenAddress: "0.0.0.0:8765", CertFile: certPath, KeyFile: keyPath}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate self-signed TLS config: %v", err)
	}
	if _, err := cfg.LoadTLSCertificate(); err != nil {
		t.Fatalf("load validated TLS certificate: %v", err)
	}
}

func TestRemoteConfigRejectsMismatchedTLSKey(t *testing.T) {
	certPath, _ := writeSelfSignedPair(t)
	_, otherKeyPath := writeSelfSignedPair(t)
	cfg := Config{ListenAddress: "0.0.0.0:8765", CertFile: certPath, KeyFile: otherKeyPath}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "TLS certificate or key") {
		t.Fatalf("Validate() error = %v, want mismatched TLS key error", err)
	}
}

func TestRemoteConfigRejectsInvalidAddress(t *testing.T) {
	for _, address := range []string{"localhost:8765", "0.0.0.0", "127.0.0.1:notaport", "127.0.0.1:65536"} {
		t.Run(address, func(t *testing.T) {
			if err := (Config{ListenAddress: address}).Validate(); err == nil {
				t.Fatal("Validate() succeeded for invalid address")
			}
		})
	}
}

func writeSelfSignedPair(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ECDSA key: %v", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "stable test remote"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create self-signed certificate: %v", err)
	}
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write private key: %v", err)
	}
	return certPath, keyPath
}
