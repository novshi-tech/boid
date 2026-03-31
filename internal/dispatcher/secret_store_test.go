package dispatcher_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/dispatcher"
)

func setupStore(t *testing.T) *dispatcher.SecretStore {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	key := dispatcher.GenerateKey()
	s, err := dispatcher.NewSecretStore(d.Conn, key)
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	return s
}

func TestStore_SetAndGet(t *testing.T) {
	s := setupStore(t)

	if err := s.Set("github/pat", "ghp_abc123"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	val, err := s.Get("github/pat")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if val != "ghp_abc123" {
		t.Errorf("Get = %q, want %q", val, "ghp_abc123")
	}
}

func TestStore_GetNotFound(t *testing.T) {
	s := setupStore(t)

	_, err := s.Get("nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent key")
	}
}

func TestStore_Update(t *testing.T) {
	s := setupStore(t)

	s.Set("key", "value1")
	s.Set("key", "value2")

	val, err := s.Get("key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if val != "value2" {
		t.Errorf("Get after update = %q, want %q", val, "value2")
	}
}

func TestStore_Delete(t *testing.T) {
	s := setupStore(t)

	s.Set("key", "value")
	if err := s.Delete("key"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err := s.Get("key")
	if err == nil {
		t.Error("expected error after delete")
	}
}

func TestStore_List(t *testing.T) {
	s := setupStore(t)

	s.Set("a/key1", "v1")
	s.Set("b/key2", "v2")
	s.Set("c/key3", "v3")

	keys, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 3 {
		t.Errorf("List returned %d keys, want 3", len(keys))
	}
}

func TestStore_EncryptionIsReal(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	key := dispatcher.GenerateKey()
	s, _ := dispatcher.NewSecretStore(d.Conn, key)
	s.Set("test", "plaintext-secret")

	// Read raw encrypted value from DB
	var encrypted []byte
	err = d.Conn.QueryRow("SELECT value_encrypted FROM secrets WHERE key = ?", "test").Scan(&encrypted)
	if err != nil {
		t.Fatalf("query raw: %v", err)
	}

	// Encrypted value should not contain the plaintext
	if string(encrypted) == "plaintext-secret" {
		t.Error("value stored as plaintext, not encrypted")
	}
}

func TestStore_WrongKeyCannotDecrypt(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	key1 := dispatcher.GenerateKey()
	s1, _ := dispatcher.NewSecretStore(d.Conn, key1)
	s1.Set("test", "secret-value")

	key2 := dispatcher.GenerateKey()
	s2, _ := dispatcher.NewSecretStore(d.Conn, key2)
	_, err = s2.Get("test")
	if err == nil {
		t.Error("expected decryption failure with wrong key")
	}
}
