package legacyrepos

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
)

// RegisterExtraRepositories registers legacy workspace repositories without contacting their upstreams.
func RegisterExtraRepositories(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT slug,extra_repos FROM workspaces WHERE extra_repos!='[]' ORDER BY slug`)
	if err != nil {
		return err
	}
	type legacyWorkspace struct{ slug, repos string }
	var workspaces []legacyWorkspace
	for rows.Next() {
		var ws legacyWorkspace
		if err := rows.Scan(&ws.slug, &ws.repos); err != nil {
			rows.Close()
			return err
		}
		workspaces = append(workspaces, ws)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, ws := range workspaces {
		var repos []json.RawMessage
		if err := json.Unmarshal([]byte(ws.repos), &repos); err != nil {
			slog.Warn("extra_repos migration: invalid list skipped", "workspace", ws.slug, "error", err)
		}
		for _, entry := range repos {
			var raw string
			if err := json.Unmarshal(entry, &raw); err != nil {
				slog.Warn("extra_repos migration: invalid entry skipped", "workspace", ws.slug)
				continue
			}
			upstream, err := legacyRepositoryURL(raw)
			if err != nil {
				slog.Warn("extra_repos migration: invalid URL skipped", "workspace", ws.slug, "error", err)
				continue
			}
			if err := registerLegacyRepository(tx, ws.slug, upstream); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`UPDATE workspaces SET extra_repos='[]' WHERE slug=?`, ws.slug); err != nil {
			return err
		}
	}
	return nil
}

func legacyRepositoryURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		if at := strings.Index(raw, "@"); at >= 0 {
			raw = raw[at+1:]
		}
		host, path, ok := strings.Cut(raw, ":")
		if !ok {
			return "", fmt.Errorf("expected a git URL")
		}
		raw = "ssh://" + host + "/" + path
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid git URL")
	}
	if u.Scheme != "https" && u.Scheme != "http" && u.Scheme != "ssh" && u.Scheme != "git" {
		return "", fmt.Errorf("unsupported git URL scheme")
	}
	if (u.Scheme == "http" || u.Scheme == "https") && u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("credential or query-bearing URL")
	}
	host := strings.ToLower(u.Host)
	if u.Scheme == "ssh" {
		host = strings.ToLower(u.Hostname())
	}
	path := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	parts := strings.Split(path, "/")
	if host == "" || len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(path, "\\@ \t\n") || parts[0] == ".." || parts[1] == ".." {
		return "", fmt.Errorf("expected host/owner/repository")
	}
	for _, part := range parts {
		if part == "." || part == ".." {
			return "", fmt.Errorf("invalid repository path")
		}
		for _, r := range part {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("_.-", r)) {
				return "", fmt.Errorf("invalid repository path")
			}
		}
	}
	return "https://" + host + "/" + path + ".git", nil
}

func registerLegacyRepository(tx *sql.Tx, workspace, upstream string) error {
	rows, err := tx.Query(`SELECT p.id,COALESCE(p.upstream_url,''),COALESCE(pw.workspace_id,'') FROM projects p LEFT JOIN project_workspaces pw ON pw.project_id=p.id ORDER BY p.id`)
	if err != nil {
		return err
	}
	id := ""
	for rows.Next() {
		var candidate, raw, owner string
		if err := rows.Scan(&candidate, &raw, &owner); err != nil {
			rows.Close()
			return err
		}
		normalized, err := legacyRepositoryURL(raw)
		if err == nil && normalized == upstream && (owner == workspace || owner == "") {
			id = candidate
			break
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if id == "" {
		sum := sha256.Sum256([]byte(workspace + "\x00" + upstream))
		suffix := hex.EncodeToString(sum[:])[:16]
		id = "extra-" + suffix
		parts := strings.Split(strings.TrimSuffix(upstream, ".git"), "/")
		name := parts[len(parts)-1]
		baseName := name
		for attempt := 0; ; attempt++ {
			var collisions int
			if err := tx.QueryRow(`SELECT count(*) FROM projects WHERE name=? OR id=?`, name, id).Scan(&collisions); err != nil {
				return err
			}
			if collisions == 0 {
				break
			}
			id = fmt.Sprintf("extra-%s-%d", suffix, attempt+1)
			name = fmt.Sprintf("%s-%s-%d", baseName, suffix, attempt+1)
			slog.Warn("extra_repos migration: repository identity collision resolved", "workspace", workspace, "id", id, "name", name)
		}
		if _, err := tx.Exec(`INSERT INTO projects(id,name,work_dir,upstream_url,perm) VALUES (?,?,'',?,'fetch')`, id, name, upstream); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`INSERT INTO project_workspaces(project_id,workspace_id) VALUES (?,?) ON CONFLICT(project_id) DO NOTHING`, id, workspace)
	return err
}
