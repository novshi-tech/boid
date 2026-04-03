CREATE TABLE IF NOT EXISTS secrets_new (
    id              TEXT PRIMARY KEY,
    namespace       TEXT NOT NULL DEFAULT 'default',
    key             TEXT NOT NULL,
    value_encrypted BLOB NOT NULL,
    created_at      DATETIME NOT NULL DEFAULT (datetime('now')),
    updated_at      DATETIME NOT NULL DEFAULT (datetime('now')),
    UNIQUE(namespace, key)
);

INSERT INTO secrets_new (id, namespace, key, value_encrypted, created_at, updated_at)
    SELECT id, 'default', key, value_encrypted, created_at, updated_at FROM secrets;

DROP TABLE secrets;

ALTER TABLE secrets_new RENAME TO secrets;
