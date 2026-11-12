// Package auth holds the shared-secret handling: key files, key ids, the
// handshake HMACs and the session-key derivation. The secret itself never
// goes on the wire; only nonces and HMAC proofs do.
package auth

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

const KeyLen = 32

// KeyID is the first byte of sha256(key). It is only a hint for rotation,
// not a secret and not a security boundary.
func KeyID(key []byte) byte {
	h := sha256.Sum256(key)
	return h[0]
}

// Generate returns 32 fresh random bytes.
func Generate() ([]byte, error) {
	k := make([]byte, KeyLen)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	return k, nil
}

// WriteKeyFile writes the key hex-encoded with mode 0600. It refuses to
// overwrite an existing file.
func WriteKeyFile(path string, key []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(hex.EncodeToString(key) + "\n")
	return err
}

// LoadKeyFile reads a hex key file. It refuses files readable by group or
// others, because a loose permission on the secret is a configuration bug.
func LoadKeyFile(path string) ([]byte, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file", path)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s: permissions %o are too open, need 0600 or stricter", path, st.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("%s: not valid hex: %w", path, err)
	}
	if len(key) != KeyLen {
		return nil, fmt.Errorf("%s: key must be %d bytes, got %d", path, KeyLen, len(key))
	}
	return key, nil
}

// Keyring maps key ids to keys so keys can be rotated without downtime.
type Keyring struct {
	keys map[byte][]byte
}

func NewKeyring(keys ...[]byte) (*Keyring, error) {
	if len(keys) == 0 {
		return nil, errors.New("keyring needs at least one key")
	}
	kr := &Keyring{keys: map[byte][]byte{}}
	for _, k := range keys {
		id := KeyID(k)
		if _, dup := kr.keys[id]; dup {
			return nil, fmt.Errorf("two keys share key id %02x, regenerate one", id)
		}
		kr.keys[id] = k
	}
	return kr, nil
}

func (kr *Keyring) Get(id byte) ([]byte, bool) {
	k, ok := kr.keys[id]
	return k, ok
}

// Mac is HMAC-SHA256(key, label || parts...). Labels give domain separation
// between the client proof, the server accept and the HTTP request tags.
func Mac(key []byte, label string, parts ...[]byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(label))
	for _, p := range parts {
		m.Write(p)
	}
	return m.Sum(nil)
}

// Equal is a constant-time comparison.
func Equal(a, b []byte) bool { return hmac.Equal(a, b) }

// SessionKey derives the per-session key from the secret and both nonces.
func SessionKey(key, nc, ns []byte) ([]byte, error) {
	salt := make([]byte, 0, len(nc)+len(ns))
	salt = append(salt, nc...)
	salt = append(salt, ns...)
	return hkdf.Key(sha256.New, key, salt, "hstun1-session", 32)
}
