#!/usr/bin/env bash
set -euo pipefail

# Step 1: set a secret and verify it
e2e_log "setting secret test-key"
echo "secret-value-1" | e2e_run "$E2E_BIN_DIR/boid" secret set test-key

e2e_log "getting secret test-key"
got="$("$E2E_BIN_DIR/boid" secret get test-key)"
e2e_assert_contains "$got" "secret-value-1"

# Step 2: list and verify key exists
e2e_log "listing secrets"
list="$("$E2E_BIN_DIR/boid" secret list)"
e2e_assert_contains "$list" "test-key"

# Step 3: overwrite and verify updated value
e2e_log "overwriting secret test-key"
echo "updated-value" | e2e_run "$E2E_BIN_DIR/boid" secret set test-key

e2e_log "getting updated secret test-key"
got="$("$E2E_BIN_DIR/boid" secret get test-key)"
e2e_assert_contains "$got" "updated-value"

# Step 4: delete and verify key is gone
e2e_log "deleting secret test-key"
e2e_run "$E2E_BIN_DIR/boid" secret delete test-key

e2e_log "listing secrets after delete"
list="$("$E2E_BIN_DIR/boid" secret list)"
if [[ "$list" == *"test-key"* ]]; then
  e2e_fail "expected test-key to be absent after delete, but found it in list"
fi
e2e_log "test-key correctly absent from list"

# Step 5: namespace isolation test
e2e_log "setting secret in custom-ns namespace"
echo "ns-value" | e2e_run "$E2E_BIN_DIR/boid" secret set -n custom-ns ns-key

e2e_log "verifying ns-key exists in custom-ns"
ns_list="$("$E2E_BIN_DIR/boid" secret list -n custom-ns)"
e2e_assert_contains "$ns_list" "ns-key"

e2e_log "verifying ns-key is absent in default namespace"
default_list="$("$E2E_BIN_DIR/boid" secret list)"
if [[ "$default_list" == *"ns-key"* ]]; then
  e2e_fail "expected ns-key to be absent in default namespace, but found it"
fi
e2e_log "namespace isolation verified"
