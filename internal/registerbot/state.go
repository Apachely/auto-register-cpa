package registerbot

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/MoeForge/moego/json"
)

// sealedStore encrypts queue credentials and captured keys before they touch disk.
type sealedStore struct {
	path string
	aead cipher.AEAD
}

func newSealedStore(dir string) (*sealedStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	keyPath := filepath.Join(dir, "state.key")
	protected, errRead := os.ReadFile(keyPath)
	var key []byte
	if errors.Is(errRead, os.ErrNotExist) {
		if _, errState := os.Stat(filepath.Join(dir, "queue.enc")); errState == nil {
			return nil, errors.New("registerbot: existing queue has no encryption key")
		}
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		protected, errRead = protectStateKey(key)
		if errRead != nil {
			return nil, errRead
		}
		if err := atomicWrite(keyPath, protected); err != nil {
			return nil, err
		}
	} else if errRead != nil {
		return nil, errRead
	} else {
		key, errRead = unprotectStateKey(protected)
		if errRead != nil {
			return nil, errors.New("registerbot: queue key cannot be opened by this user")
		}
	}
	block, errCipher := aes.NewCipher(key)
	clear(key)
	if errCipher != nil {
		return nil, errCipher
	}
	aead, errGCM := cipher.NewGCM(block)
	if errGCM != nil {
		return nil, errGCM
	}
	return &sealedStore{path: filepath.Join(dir, "queue.enc"), aead: aead}, nil
}

func (s *sealedStore) Save(value any) error {
	plain, err := json.Marshal(value)
	if err != nil {
		return err
	}
	defer clear(plain)
	nonce := make([]byte, s.aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return err
	}
	sealed := s.aead.Seal(nonce, nonce, plain, []byte("cpa-registerbot-v1"))
	return atomicWrite(s.path, sealed)
}

func (s *sealedStore) Load(out any) error {
	sealed, errRead := os.ReadFile(s.path)
	if errors.Is(errRead, os.ErrNotExist) {
		return nil
	}
	if errRead != nil {
		return errRead
	}
	n := s.aead.NonceSize()
	if len(sealed) < n {
		return errors.New("registerbot: truncated encrypted queue")
	}
	plain, errOpen := s.aead.Open(nil, sealed[:n], sealed[n:], []byte("cpa-registerbot-v1"))
	if errOpen != nil {
		return errors.New("registerbot: encrypted queue authentication failed")
	}
	defer clear(plain)
	if err := json.Unmarshal(plain, out); err != nil {
		return fmt.Errorf("registerbot: invalid queue state: %w", err)
	}
	return nil
}

// atomicWrite never removes the old destination before the replacement succeeds.
func atomicWrite(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".registerbot-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer func() { _ = os.Remove(name) }()
	if err = file.Chmod(0o600); err != nil {
		_ = file.Close()
		return err
	}
	if _, err = file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
