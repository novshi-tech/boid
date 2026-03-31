package project

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/novshi-tech/boid/internal/db"
)

type scanner interface {
	Scan(dest ...any) error
}

// CreateProject inserts a new project record.
func CreateProject(dbtx db.DBTX, p *Project) error {
	now := time.Now().UTC()
	p.CreatedAt = now
	p.UpdatedAt = now
	_, err := dbtx.Exec(
		`INSERT INTO projects (id, work_dir, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		p.ID, p.WorkDir, p.CreatedAt, p.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert project: %w", err)
	}
	return nil
}

// GetProject retrieves a project by ID.
func GetProject(dbtx db.DBTX, id string) (*Project, error) {
	row := dbtx.QueryRow(
		`SELECT id, work_dir, created_at, updated_at FROM projects WHERE id = ?`, id,
	)
	return scanProject(row)
}

// ListProjects returns all projects ordered by creation time.
func ListProjects(dbtx db.DBTX) ([]*Project, error) {
	rows, err := dbtx.Query(
		`SELECT id, work_dir, created_at, updated_at FROM projects ORDER BY created_at`,
	)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()
	return scanProjects(rows)
}

// DeleteProject removes a project by ID.
func DeleteProject(dbtx db.DBTX, id string) error {
	res, err := dbtx.Exec(`DELETE FROM projects WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete project: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("project %q not found", id)
	}
	return nil
}

func scanProject(s scanner) (*Project, error) {
	var p Project
	if err := s.Scan(&p.ID, &p.WorkDir, &p.CreatedAt, &p.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("project not found")
		}
		return nil, fmt.Errorf("scan project: %w", err)
	}
	return &p, nil
}

func scanProjects(rows *sql.Rows) ([]*Project, error) {
	var projects []*Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}
