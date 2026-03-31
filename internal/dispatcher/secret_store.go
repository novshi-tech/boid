package dispatcher

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"fmt"
	"io"
)

// SecretStore provides encrypted secret storage backed by SQLite.
type SecretStore struct {
	db  *sql.DB
	gcm cipher.AEAD
}

// GenerateKey creates a random 32-byte AES-256 key.
func GenerateKey() []byte {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic("failed to generate key: " + err.Error())
	}
	return key
}

// NewSecretStore creates a store with the given database and encryption key.
func NewSecretStore(d *sql.DB, key []byte) (*SecretStore, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	return &SecretStore{db: d, gcm: gcm}, nil
}

func (s *SecretStore) encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, s.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return s.gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func (s *SecretStore) decrypt(ciphertext []byte) ([]byte, error) {
	nonceSize := s.gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("ciphertext too short")
	}
	nonce, data := ciphertext[:nonceSize], ciphertext[nonceSize:]
	return s.gcm.Open(nil, nonce, data, nil)
}

func (s *SecretStore) Set(key, value string) error {
	encrypted, err := s.encrypt([]byte(value))
	if err != nil {
		return fmt.Errorf("encrypt: %w", err)
	}

	_, err = s.db.Exec(`
		INSERT INTO secrets (id, key, value_encrypted)
		VALUES (lower(hex(randomblob(8))), ?, ?)
		ON CONFLICT(key) DO UPDATE SET
			value_encrypted = excluded.value_encrypted,
			updated_at = datetime('now')
	`, key, encrypted)
	return err
}

func (s *SecretStore) Get(key string) (string, error) {
	var encrypted []byte
	err := s.db.QueryRow("SELECT value_encrypted FROM secrets WHERE key = ?", key).Scan(&encrypted)
	if err != nil {
		return "", fmt.Errorf("secret %q: %w", key, err)
	}

	plaintext, err := s.decrypt(encrypted)
	if err != nil {
		return "", fmt.Errorf("decrypt %q: %w", key, err)
	}
	return string(plaintext), nil
}

func (s *SecretStore) Delete(key string) error {
	_, err := s.db.Exec("DELETE FROM secrets WHERE key = ?", key)
	return err
}

func (s *SecretStore) List() ([]string, error) {
	rows, err := s.db.Query("SELECT key FROM secrets ORDER BY key")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}
