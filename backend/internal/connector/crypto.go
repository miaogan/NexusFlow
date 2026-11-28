package connector

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

const plainPrefix = "plain:"

// Cipher encrypts connector configuration at rest with AES-256-GCM. The key is
// the SHA-256 of the configured secret, so any sufficiently long passphrase
// yields a valid 32-byte key.
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher returns nil when secret is empty: development deployments store
// configuration as plain JSON with an explicit prefix and a warning.
func NewCipher(secret string) *Cipher {
	if strings.TrimSpace(secret) == "" {
		return nil
	}
	key := sha256.Sum256([]byte(secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil
	}
	return &Cipher{aead: aead}
}

// Encrypt returns a versioned, base64-encoded nonce+ciphertext string.
func (c *Cipher) Encrypt(plain []byte) (string, error) {
	if c == nil {
		return plainPrefix + string(plain), nil
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("connector: nonce: %w", err)
	}
	sealed := c.aead.Seal(nonce, nonce, plain, nil)
	return "v1:" + base64.StdEncoding.EncodeToString(sealed), nil
}

// Decrypt reverses Encrypt and also accepts development plaintext values.
func (c *Cipher) Decrypt(value string) ([]byte, error) {
	if strings.HasPrefix(value, plainPrefix) {
		return []byte(strings.TrimPrefix(value, plainPrefix)), nil
	}
	if c == nil {
		return nil, errors.New("connector: configuration is encrypted but no encryption key is configured")
	}
	if !strings.HasPrefix(value, "v1:") {
		return nil, errors.New("connector: unsupported configuration encoding")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, "v1:"))
	if err != nil {
		return nil, fmt.Errorf("connector: decode configuration: %w", err)
	}
	size := c.aead.NonceSize()
	if len(raw) <= size {
		return nil, errors.New("connector: encrypted configuration is truncated")
	}
	plain, err := c.aead.Open(nil, raw[:size], raw[size:], nil)
	if err != nil {
		return nil, fmt.Errorf("connector: decrypt configuration: %w", err)
	}
	return plain, nil
}

// IsPlain reports whether a stored value is development plaintext.
func IsPlain(value string) bool {
	return strings.HasPrefix(value, plainPrefix)
}
