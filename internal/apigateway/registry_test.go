package apigateway

import "testing"

func TestRegistry_RegisterAndAuthorize(t *testing.T) {
	r := NewRegistry()
	token := r.Register(RegisterInput{Services: []string{"myapp", "ops"}, Namespace: "ws-a", TaskID: "task-1", ReadOnly: false})

	allowed, valid := r.Authorize(token, "myapp")
	if !valid || !allowed {
		t.Fatalf("Authorize(myapp) = (%v, %v), want (true, true)", allowed, valid)
	}
	allowed, valid = r.Authorize(token, "ops")
	if !valid || !allowed {
		t.Fatalf("Authorize(ops) = (%v, %v), want (true, true)", allowed, valid)
	}
	allowed, valid = r.Authorize(token, "not-enabled")
	if !valid || allowed {
		t.Fatalf("Authorize(not-enabled) = (%v, %v), want (false, true)", allowed, valid)
	}
}

func TestRegistry_UnknownToken(t *testing.T) {
	r := NewRegistry()
	allowed, valid := r.Authorize("nonexistent", "myapp")
	if valid || allowed {
		t.Fatalf("Authorize on unknown token = (%v, %v), want (false, false)", allowed, valid)
	}
}

func TestRegistry_Unregister(t *testing.T) {
	r := NewRegistry()
	token := r.Register(RegisterInput{Services: []string{"myapp"}, Namespace: "ws-a", TaskID: "task-1", ReadOnly: false})
	r.Unregister(token)

	_, valid := r.Authorize(token, "myapp")
	if valid {
		t.Fatalf("Authorize after Unregister: tokenValid = true, want false")
	}
}

func TestRegistry_UnregisterUnknownTokenIsNoop(t *testing.T) {
	r := NewRegistry()
	r.Unregister("never-registered") // must not panic
}

func TestRegistry_Lookup(t *testing.T) {
	r := NewRegistry()
	token := r.RegisterToken("fixed-token", RegisterInput{Services: []string{"myapp"}, Namespace: "ws-a", TaskID: "task-1", ReadOnly: true})
	if token != "fixed-token" {
		t.Fatalf("RegisterToken returned %q, want %q", token, "fixed-token")
	}

	entry, ok := r.Lookup("fixed-token")
	if !ok {
		t.Fatal("Lookup: not found")
	}
	if entry.Namespace != "ws-a" {
		t.Errorf("entry.Namespace = %q, want %q", entry.Namespace, "ws-a")
	}
	if entry.TaskID != "task-1" {
		t.Errorf("entry.TaskID = %q, want %q", entry.TaskID, "task-1")
	}
	if !entry.ReadOnly {
		t.Errorf("entry.ReadOnly = false, want true")
	}
	if !entry.Services["myapp"] {
		t.Errorf("entry.Services[myapp] = false, want true")
	}
}

func TestRegistry_NilRegistryFailsClosed(t *testing.T) {
	var r *Registry
	allowed, valid := r.Authorize("any", "myapp")
	if valid || allowed {
		t.Fatalf("nil Registry.Authorize = (%v, %v), want (false, false)", allowed, valid)
	}
	entry, ok := r.Lookup("any")
	if ok {
		t.Fatalf("nil Registry.Lookup: ok = true, want false")
	}
	if entry.Token != "" {
		t.Fatalf("nil Registry.Lookup: entry = %+v, want zero value", entry)
	}
}

func TestRegistry_DuplicateServiceNamesDeduped(t *testing.T) {
	r := NewRegistry()
	token := r.Register(RegisterInput{Services: []string{"myapp", "myapp", "ops"}, Namespace: "ws-a", TaskID: "", ReadOnly: false})
	entry, ok := r.Lookup(token)
	if !ok {
		t.Fatal("Lookup: not found")
	}
	if len(entry.Services) != 2 {
		t.Errorf("len(entry.Services) = %d, want 2 (deduped)", len(entry.Services))
	}
}
