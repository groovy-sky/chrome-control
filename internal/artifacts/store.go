// Package artifacts stores task artifacts under opaque random identifiers.
// It is the only component that maps an artifact ID to a filesystem path.
package artifacts

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MaxFileSizeBytes is the largest artifact accepted by the store.
const MaxFileSizeBytes = 10 * 1024 * 1024

// idLength is the number of hex characters in a generated artifact ID.
const idLength = 32

// Store persists artifacts under a single root directory.
type Store struct {
	root string
}

// New creates the artifact root (mode 0700) and returns a Store.
func New(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	return &Store{root: abs}, nil
}

// Root returns the absolute artifact root directory.
func (s *Store) Root() string { return s.root }

// Save writes data under an opaque random ID and returns the ID.
func (s *Store) Save(data []byte) (string, error) {
	if len(data) > MaxFileSizeBytes {
		return "", fmt.Errorf("artifact exceeds %d bytes", MaxFileSizeBytes)
	}
	id, err := randomID()
	if err != nil {
		return "", err
	}
	path, err := s.resolve(id)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return id, nil
}

// Delete removes the artifact with the given ID. Removing an artifact that no
// longer exists is not an error.
func (s *Store) Delete(id string) error {
	path, err := s.resolve(id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// resolve maps an artifact ID to an absolute path inside the root, rejecting
// anything that is not an opaque identifier or that escapes the root.
func (s *Store) resolve(id string) (string, error) {
	if !validID(id) {
		return "", fmt.Errorf("invalid artifact id")
	}
	path := filepath.Join(s.root, id)
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	absRoot, err := filepath.Abs(s.root)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(absPath, absRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("invalid artifact path")
	}
	return absPath, nil
}

// validID reports whether id is a plain lowercase hex identifier of the
// expected length, which by construction cannot traverse directories.
func validID(id string) bool {
	if len(id) != idLength {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func randomID() (string, error) {
	b := make([]byte, idLength/2)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
