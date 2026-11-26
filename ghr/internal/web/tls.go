package web

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Rightbracket/usb-gha-runner/ghr/internal/config"
)

// EnsureCert creates a self-signed certificate on first boot and returns its
// SHA-256 fingerprint, which the console shows so users can check it in the
// browser's certificate warning.
func EnsureCert(dir, hostname string) (certFile, keyFile, fingerprint string, err error) {
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if _, err := tls.LoadX509KeyPair(certFile, keyFile); err != nil {
		if err := generate(certFile, keyFile, hostname); err != nil {
			return "", "", "", err
		}
	}
	b, err := os.ReadFile(certFile)
	if err != nil {
		return "", "", "", err
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return "", "", "", fmt.Errorf("%s is not PEM", certFile)
	}
	sum := sha256.Sum256(blk.Bytes)
	fingerprint = colonHex(sum[:])
	_ = config.WriteFileAtomic(filepath.Join(dir, "fingerprint.txt"), []byte(fingerprint+"\n"), 0o644)
	return certFile, keyFile, fingerprint, nil
}

func generate(certFile, keyFile, hostname string) error {
	if err := os.MkdirAll(filepath.Dir(certFile), 0o700); err != nil {
		return err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	names := []string{"localhost"}
	if hostname != "" {
		names = append([]string{hostname, hostname + ".local"}, names...)
	}
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: names[0], Organization: []string{"USB GHA runner"}},
		DNSNames:              names,
		NotBefore:             time.Now().Add(-24 * time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	kb, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	if err := config.WriteFileAtomic(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		return err
	}
	return config.WriteFileAtomic(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644)
}

func colonHex(b []byte) string {
	parts := make([]string, len(b))
	for i, c := range b {
		parts[i] = fmt.Sprintf("%02X", c)
	}
	return strings.Join(parts, ":")
}
