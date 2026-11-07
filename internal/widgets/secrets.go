package widgets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
)

// Keys at rest (SPEC-status-widgets AC-004, AC-005, §6.4).
//
// A widget key is sealed with AES-256-GCM under the server key from
// HOMEPAD_SECRET_KEY (32 bytes as 64 hex characters), with a fresh random
// 12-byte nonce per write. The additional data binds the ciphertext to its
// widget and to the address it was entered for (Stored.SecretAAD), so a
// ciphertext copied onto another row, or a key whose tile moved to another
// host, won't decrypt.
//
// Key rotation is out of scope: a changed server key makes every saved key
// secret_unreadable, and the admin re-enters them.

// SecretKeyEnv is the env var that holds the server key.
const SecretKeyEnv = "HOMEPAD_SECRET_KEY"

// Sealer seals and opens widget keys. A nil *Sealer means "no server key":
// Seal fails with ErrNoSecretKey and Open with ErrSecretUnreadable.
type Sealer struct{ aead cipher.AEAD }

var errMalformedKey = errors.New(SecretKeyEnv + " must be 64 hex characters (32 bytes); widget keys can't be saved until it is fixed")

// ParseSecretKey reads the server key. Empty → (nil, nil): widgets that need
// no key still work. Malformed → (nil, error); the error never contains the
// value, so it is safe to log.
func ParseSecretKey(s string) (*Sealer, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if len(s) != 64 {
		return nil, errMalformedKey
	}
	key, err := hex.DecodeString(s)
	if err != nil {
		return nil, errMalformedKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, errMalformedKey
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errMalformedKey
	}
	return &Sealer{aead: aead}, nil
}

// Seal encrypts plaintext with a fresh nonce.
func (s *Sealer) Seal(plaintext, aad []byte) (ciphertext, nonce []byte, err error) {
	if s == nil {
		return nil, nil, ErrNoSecretKey
	}
	nonce = make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return s.aead.Seal(nil, nonce, plaintext, aad), nonce, nil
}

// Open decrypts a sealed key. Any failure — no server key, a changed key, a
// ciphertext moved to another widget or address — is ErrSecretUnreadable.
func (s *Sealer) Open(ciphertext, nonce, aad []byte) ([]byte, error) {
	if s == nil || len(nonce) != s.aead.NonceSize() {
		return nil, ErrSecretUnreadable
	}
	out, err := s.aead.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, ErrSecretUnreadable
	}
	return out, nil
}
