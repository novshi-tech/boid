ALTER TABLE jobs ADD COLUMN terminal_title TEXT NOT NULL DEFAULT '';
ALTER TABLE jobs ADD COLUMN display_name_default INTEGER NOT NULL DEFAULT 0;
-- Existing names have no provenance: preserve them as explicit names.
