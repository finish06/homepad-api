package widgets

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SPEC-status-widgets AC-004, AC-005, §6.4 — keys at rest are AES-256-GCM with
// a key from HOMEPAD_SECRET_KEY and a fresh 12-byte nonce per write.

const testKeyA = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
const testKeyB = "1f1e1d1c1b1a191817161514131211100f0e0d0c0b0a09080706050403020100"

func TestParseSecretKey(t *testing.T) {
	s, err := ParseSecretKey("")
	assert.NoError(t, err)
	assert.Nil(t, s, "unset → no sealer, widgets without keys still work")

	s, err = ParseSecretKey("  " + testKeyA + "\n")
	require.NoError(t, err)
	assert.NotNil(t, s)

	upper := strings.ToUpper(testKeyA)
	s, err = ParseSecretKey(upper)
	require.NoError(t, err)
	assert.NotNil(t, s)

	for _, bad := range []string{
		"abc",
		testKeyA[:62],
		testKeyA + "00",
		"zz" + testKeyA[2:],
		"my-very-secret-passphrase-that-is-not-hex-but-is-sixty-four-ch!",
	} {
		s, err := ParseSecretKey(bad)
		assert.Error(t, err, "%q", bad)
		assert.Nil(t, s)
		if err != nil {
			assert.NotContains(t, err.Error(), bad, "the error never echoes the value")
		}
	}
}

func TestSealer_RoundTripWithFreshNonce(t *testing.T) {
	s, err := ParseSecretKey(testKeyA)
	require.NoError(t, err)
	plain := []byte("jf-api-key-0123456789")
	aad := []byte("service-1")

	ct1, n1, err := s.Seal(plain, aad)
	require.NoError(t, err)
	ct2, n2, err := s.Seal(plain, aad)
	require.NoError(t, err)

	assert.Len(t, n1, 12)
	assert.Len(t, n2, 12)
	assert.NotEqual(t, n1, n2, "a random nonce per write")
	assert.NotEqual(t, ct1, ct2)
	assert.False(t, bytes.Contains(ct1, plain), "ciphertext holds no plaintext")

	got, err := s.Open(ct1, n1, aad)
	require.NoError(t, err)
	assert.Equal(t, plain, got)
}

func TestSealer_WrongAADOrKeyIsUnreadable(t *testing.T) {
	a, _ := ParseSecretKey(testKeyA)
	b, _ := ParseSecretKey(testKeyB)
	ct, nonce, err := a.Seal([]byte("k"), []byte("service-1"))
	require.NoError(t, err)

	_, err = a.Open(ct, nonce, []byte("service-2"))
	assert.ErrorIs(t, err, ErrSecretUnreadable, "a ciphertext copied to another widget won't decrypt")

	_, err = b.Open(ct, nonce, []byte("service-1"))
	assert.ErrorIs(t, err, ErrSecretUnreadable, "a changed server key can't read old keys")

	_, err = a.Open(ct, nonce[:5], []byte("service-1"))
	assert.ErrorIs(t, err, ErrSecretUnreadable)

	var none *Sealer
	_, err = none.Open(ct, nonce, []byte("service-1"))
	assert.ErrorIs(t, err, ErrSecretUnreadable, "a lost server key can't read old keys")
	_, _, err = none.Seal([]byte("k"), nil)
	assert.ErrorIs(t, err, ErrNoSecretKey)
}
