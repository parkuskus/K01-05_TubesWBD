package database

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
)

type credentialCipher struct{ aead cipher.AEAD }

func newCredentialCipher(key []byte) (*credentialCipher, error) {
	if len(key) != 32 {
		return nil, ErrInvalidConfig
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrInvalidConfig
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, ErrInvalidConfig
	}
	return &credentialCipher{aead: aead}, nil
}

// Format: version byte, random nonce, authenticated ciphertext.
func (c *credentialCipher) seal(password string) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, ErrUnavailable
	}
	data := append([]byte{1}, nonce...)
	return c.aead.Seal(data, nonce, []byte(password), []byte("if3110.database.password.v1")), nil
}

func (c *credentialCipher) open(data []byte) (string, error) {
	n := c.aead.NonceSize()
	if len(data) < 1+n+c.aead.Overhead() || data[0] != 1 {
		return "", ErrCredentials
	}
	plaintext, err := c.aead.Open(nil, data[1:1+n], data[1+n:], []byte("if3110.database.password.v1"))
	if err != nil {
		return "", ErrCredentials
	}
	return string(plaintext), nil
}
