package cloud

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// loadOrCreateHubKey returns the hub's SSH identity, stored in the data directory.
// Cloud servers accept only this key, and only for the tunnel.
func loadOrCreateHubKey(dir string) (ssh.Signer, string, error) {
	path := filepath.Join(dir, "hub_ed25519")
	if data, err := os.ReadFile(path); err == nil {
		signer, err := ssh.ParsePrivateKey(data)
		if err != nil {
			return nil, "", fmt.Errorf("read %s: %w", path, err)
		}
		return signer, authorizedKey(signer.PublicKey(), "chiperka-hub"), nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, "", err
	}
	privPEM, _, signer, err := newKey("chiperka-hub")
	if err != nil {
		return nil, "", err
	}
	if err := os.WriteFile(path, privPEM, 0o600); err != nil {
		return nil, "", err
	}
	return signer, authorizedKey(signer.PublicKey(), "chiperka-hub"), nil
}

// newKey generates an ed25519 key pair: OpenSSH private key PEM, public key in
// authorized_keys format, and a signer.
func newKey(comment string) ([]byte, string, ssh.Signer, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", nil, err
	}
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return nil, "", nil, err
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		return nil, "", nil, err
	}
	return pem.EncodeToMemory(block), authorizedKey(signer.PublicKey(), comment), signer, nil
}

func authorizedKey(k ssh.PublicKey, comment string) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))) + " " + comment
}
