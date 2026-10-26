package plugin

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Vault seals account form values with AES-256-GCM.
type Vault struct {
	aead cipher.AEAD
}

// NewVault builds a Vault from the base64 encoding of a 32-byte key
// (PLUGIN_SECRET_KEY, e.g. from `openssl rand -base64 32`).
func NewVault(base64Key string) (*Vault, error) {
	base64Key = strings.TrimSpace(base64Key)
	if base64Key == "" {
		return nil, errors.New("plugin: vault: key is empty")
	}
	var key []byte
	var err error
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if key, err = enc.DecodeString(base64Key); err == nil {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("plugin: vault: key is not base64: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("plugin: vault: key is %d bytes, want 32", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("plugin: vault: cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("plugin: vault: gcm: %w", err)
	}
	return &Vault{aead: aead}, nil
}

// Seal encrypts values and returns nonce||ciphertext. bind (the account's
// user key and plugin) is authenticated but not stored, so a sealed value
// copied to another person's row does not open.
func (v *Vault) Seal(values map[string]string, bind string) ([]byte, error) {
	plain, err := json.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("plugin: vault: marshal: %w", err)
	}
	nonce := make([]byte, v.aead.NonceSize(), v.aead.NonceSize()+len(plain)+v.aead.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("plugin: vault: nonce: %w", err)
	}
	return v.aead.Seal(nonce, nonce, plain, []byte(bind)), nil
}

// Open decrypts a value made by Seal with the same bind.
func (v *Vault) Open(sealed []byte, bind string) (map[string]string, error) {
	n := v.aead.NonceSize()
	if len(sealed) < n+v.aead.Overhead() {
		return nil, errors.New("plugin: vault: sealed value is too short")
	}
	plain, err := v.aead.Open(nil, sealed[:n], sealed[n:], []byte(bind))
	if err != nil {
		return nil, fmt.Errorf("plugin: vault: open: %w", err)
	}
	var values map[string]string
	if err := json.Unmarshal(plain, &values); err != nil {
		return nil, fmt.Errorf("plugin: vault: unmarshal: %w", err)
	}
	return values, nil
}
