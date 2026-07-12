package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
)

const (
	// EnvMasterKey is the environment variable name for the AES-256 master key.
	// Must be a 32-byte base64-encoded string.
	EnvMasterKey = "AIIMS_MASTER_KEY"

	// nonceSize is the GCM nonce size (12 bytes is standard).
	nonceSize = 12
)

// Enabled returns true if the AIIMS_MASTER_KEY env var is set.
func Enabled() bool {
	return os.Getenv(EnvMasterKey) != ""
}

// masterKey decodes and validates the master key from the environment.
func masterKey() ([]byte, error) {
	encoded := os.Getenv(EnvMasterKey)
	if encoded == "" {
		return nil, errors.New("AIIMS_MASTER_KEY not set")
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("invalid base64 in AIIMS_MASTER_KEY: %v", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("AIIMS_MASTER_KEY must be 32 bytes (got %d)", len(key))
	}
	return key, nil
}

// Encrypt encrypts plaintext using AES-256-GCM with the master key from env.
// Returns base64(nonce || ciphertext || tag). Returns plaintext unchanged if
// AIIMS_MASTER_KEY is not set.
func Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}

	key, err := masterKey()
	if err != nil {
		// No master key — return plaintext unchanged (backward compatible)
		return plaintext, nil
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("aes.NewCipher: %v", err)
	}

	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("cipher.NewGCM: %v", err)
	}

	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("rand.Read: %v", err)
	}

	// Seal appends ciphertext+tag to nonce
	ciphertext := aesGCM.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// Decrypt decrypts a value encrypted by Encrypt. If the value doesn't look
// encrypted (base64 decode fails or AIIMS_MASTER_KEY is not set), returns
// the value unchanged for backward compatibility.
func Decrypt(ciphertext string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}

	key, err := masterKey()
	if err != nil {
		// No master key — return as-is
		return ciphertext, nil
	}

	data, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		// Not base64 — assume plaintext (unencrypted legacy data)
		return ciphertext, nil
	}

	if len(data) < nonceSize+16 { // nonce + minimum GCM tag
		return ciphertext, nil
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("aes.NewCipher: %v", err)
	}

	aesGCM, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("cipher.NewGCM: %v", err)
	}

	nonce := data[:nonceSize]
	ciphertextBytes := data[nonceSize:]

	plaintext, err := aesGCM.Open(nil, nonce, ciphertextBytes, nil)
	if err != nil {
		// Decryption failed — could be wrong key or unencrypted data
		return "", fmt.Errorf("decryption failed (wrong key?): %v", err)
	}

	return string(plaintext), nil
}

// SecureWipe overwrites a byte slice with zeros. Use for sensitive data
// that should be cleared from memory after use.
func SecureWipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// WipeString converts a string to bytes, wipes them, and returns empty string.
// Note: Go strings are immutable so this only wipes the mutable copy.
func WipeString(s string) string {
	if s == "" {
		return ""
	}
	b := []byte(s)
	SecureWipe(b)
	return ""
}
