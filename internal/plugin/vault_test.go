package plugin

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"testing"
)

func testKey(t *testing.T) string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(key)
}

func TestNewVault(t *testing.T) {
	key := testKey(t)
	raw, _ := base64.StdEncoding.DecodeString(key)
	tests := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{"standard base64", key, false},
		{"url base64 without padding", base64.RawURLEncoding.EncodeToString(raw), false},
		{"surrounding whitespace", "  " + key + "\n", false},
		{"empty", "", true},
		{"not base64", "not base64 !!", true},
		{"16-byte key", base64.StdEncoding.EncodeToString(raw[:16]), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewVault(tt.key)
			if (err != nil) != tt.wantErr {
				t.Errorf("NewVault() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestVaultSealOpen(t *testing.T) {
	v, err := NewVault(testKey(t))
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{"username": "hong", "password": "s3cret-pass"}
	sealed, err := v.Seal(values, "slack:U1\x00daouoffice")
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	if bytes.Contains(sealed, []byte("s3cret-pass")) || bytes.Contains(sealed, []byte("hong")) {
		t.Fatal("sealed value contains plaintext")
	}
	again, _ := v.Seal(values, "slack:U1\x00daouoffice")
	if bytes.Equal(sealed, again) {
		t.Error("two seals of the same values are identical; nonce is not random")
	}

	got, err := v.Open(sealed, "slack:U1\x00daouoffice")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if got["username"] != "hong" || got["password"] != "s3cret-pass" || len(got) != 2 {
		t.Errorf("Open() = %v", got)
	}

	other, _ := NewVault(testKey(t))
	tampered := bytes.Clone(sealed)
	tampered[len(tampered)-1] ^= 1

	tests := []struct {
		name   string
		vault  *Vault
		sealed []byte
		bind   string
	}{
		{"wrong key", other, sealed, "slack:U1\x00daouoffice"},
		{"tampered ciphertext", v, tampered, "slack:U1\x00daouoffice"},
		{"copied to another user", v, sealed, "slack:U2\x00daouoffice"},
		{"too short", v, sealed[:8], "slack:U1\x00daouoffice"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.vault.Open(tt.sealed, tt.bind); err == nil {
				t.Error("Open() succeeded, want error")
			}
		})
	}
}
