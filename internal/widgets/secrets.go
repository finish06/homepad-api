package widgets

// RED stub — implemented in the GREEN commit.

type Sealer struct{}

func ParseSecretKey(s string) (*Sealer, error) { return nil, nil }

func (s *Sealer) Seal(plaintext, aad []byte) (ciphertext, nonce []byte, err error) {
	return nil, nil, ErrNoSecretKey
}

func (s *Sealer) Open(ciphertext, nonce, aad []byte) ([]byte, error) {
	return nil, ErrSecretUnreadable
}
