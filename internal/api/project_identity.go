package api

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

const urlProjectIDPrefix = orchestrator.URLDerivedProjectIDPrefix

func deriveProjectIDFromURL(normalizedURL string) (string, error) {
	slug, err := dispatcher.RepoSlugFromOriginURL(normalizedURL)
	if err != nil {
		return "", fmt.Errorf("cannot derive a project id from %q: %w (use an HTTPS or SSH git URL)", dispatcher.SanitizeURLForLogging(normalizedURL), err)
	}
	sum := sha256.Sum256([]byte(slug))
	return urlProjectIDPrefix + hex.EncodeToString(sum[:])[:16], nil
}

func DeriveProjectIDFromURL(normalizedURL string) (string, error) {
	return deriveProjectIDFromURL(normalizedURL)
}

func (s *ProjectAppService) synthesizeProjectMeta(gitURL, name, workspaceSlug string, nameWasExplicit bool) (*orchestrator.ProjectMeta, error) {
	id, err := deriveProjectIDFromURL(gitURL)
	if err != nil {
		return nil, &StatusError{Code: http.StatusBadRequest, Message: err.Error()}
	}

	snapshot, err := s.snapshotRegisteredProjects()
	if err != nil {
		return nil, err
	}
	if matches := resolveProjectByNameExact(snapshot, name); len(matches) > 0 {
		matchIDs := make([]string, len(matches))
		for i, m := range matches {
			matchIDs[i] = m.ID
		}
		if !nameWasExplicit {
			return nil, &StatusError{Code: http.StatusConflict, Message: fmt.Sprintf(
				"project name %q derived from the URL already matches %d registered project(s) (%v); pass --name to choose a distinct name (workspace apply/export identify projects by name, and a collision detaches the wrong one)",
				name, len(matches), matchIDs,
			)}
		}
		slog.Warn("project registration: --name collides with an already-registered project's name; workspace export/apply name resolution may become ambiguous",
			"name", name, "colliding_project_ids", matchIDs)
	}

	nameSource := "url"
	if nameWasExplicit {
		nameSource = "explicit"
	}
	return &orchestrator.ProjectMeta{ID: id, Name: name, NameSource: nameSource}, nil
}
