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

	if err := s.Set("default", "GH_TOKEN", "ghp_abc123"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	val, err := s.Get("default", "GH_TOKEN")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if val != "ghp_abc123" {
		t.Errorf("Get = %q, want %q", val, "ghp_abc123")
	}
}

func TestStore_GetNotFound(t *testing.T) {
	s := setupStore(t)

	_, err := s.Get("default", "nonexistent")
	if err == nil {
		t.Error("expected error for nonexistent key")
	}
}

func TestStore_Update(t *testing.T) {
	s := setupStore(t)

	s.Set("default", "key", "value1")
	s.Set("default", "key", "value2")

	val, err := s.Get("default", "key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if val != "value2" {
		t.Errorf("Get after update = %q, want %q", val, "value2")
	}
}

func TestStore_Delete(t *testing.T) {
	s := setupStore(t)

	s.Set("default", "key", "value")
	if err := s.Delete("default", "key"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err := s.Get("default", "key")
	if err == nil {
		t.Error("expected error after delete")
	}
}

func TestStore_List(t *testing.T) {
	s := setupStore(t)

	s.Set("default", "KEY1", "v1")
	s.Set("default", "KEY2", "v2")
	s.Set("default", "KEY3", "v3")

	keys, err := s.List("default")
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
	s.Set("default", "test", "plaintext-secret")

	var encrypted []byte
	err = d.Conn.QueryRow("SELECT value_encrypted FROM secrets WHERE namespace = 'default' AND key = ?", "test").Scan(&encrypted)
	if err != nil {
		t.Fatalf("query raw: %v", err)
	}

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
	s1.Set("default", "test", "secret-value")

	key2 := dispatcher.GenerateKey()
	s2, _ := dispatcher.NewSecretStore(d.Conn, key2)
	_, err = s2.Get("default", "test")
	if err == nil {
		t.Error("expected decryption failure with wrong key")
	}
}

func TestStore_NamespaceIsolation(t *testing.T) {
	s := setupStore(t)

	s.Set("ns-a", "GH_TOKEN", "token-a")
	s.Set("ns-b", "GH_TOKEN", "token-b")

	valA, err := s.Get("ns-a", "GH_TOKEN")
	if err != nil {
		t.Fatalf("Get ns-a: %v", err)
	}
	if valA != "token-a" {
		t.Errorf("ns-a = %q, want %q", valA, "token-a")
	}

	valB, err := s.Get("ns-b", "GH_TOKEN")
	if err != nil {
		t.Fatalf("Get ns-b: %v", err)
	}
	if valB != "token-b" {
		t.Errorf("ns-b = %q, want %q", valB, "token-b")
	}

	// ns-a should not see ns-b's secrets
	keysA, _ := s.List("ns-a")
	if len(keysA) != 1 {
		t.Errorf("ns-a List = %d keys, want 1", len(keysA))
	}
}

func TestStore_EmptyNamespaceDefaultsToDefault(t *testing.T) {
	s := setupStore(t)

	s.Set("", "KEY", "val")

	val, err := s.Get("default", "KEY")
	if err != nil {
		t.Fatalf("Get with explicit default: %v", err)
	}
	if val != "val" {
		t.Errorf("Get = %q, want %q", val, "val")
	}

	val2, err := s.Get("", "KEY")
	if err != nil {
		t.Fatalf("Get with empty namespace: %v", err)
	}
	if val2 != "val" {
		t.Errorf("Get = %q, want %q", val2, "val")
	}
}
