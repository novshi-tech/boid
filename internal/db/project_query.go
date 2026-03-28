package db

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/novshi-tech/boid/internal/model"
)

func (d *DB) CreateProject(p *model.Project) error {
	now := time.Now().UTC()
	p.CreatedAt = now
	p.UpdatedAt = now

	_, err := d.Conn.Exec(
		`INSERT INTO projects (id, work_dir, created_at, updated_at)
		 VALUES (?, ?, ?, ?)`,
		p.ID, p.WorkDir, p.CreatedAt, p.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert project: %w", err)
	}
	return nil
}

func (d *DB) GetProject(id string) (*model.Project, error) {
	row := d.Conn.QueryRow(
		`SELECT id, work_dir, created_at, updated_at FROM projects WHERE id = ?`, id,
	)
	return scanProject(row)
}

func (d *DB) ListProjects() ([]*model.Project, error) {
	rows, err := d.Conn.Query(
		`SELECT id, work_dir, created_at, updated_at FROM projects ORDER BY created_at`,
	)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()
	return scanProjects(rows)
}

func (d *DB) DeleteProject(id string) error {
	res, err := d.Conn.Exec(`DELETE FROM projects WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete project: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("project %q not found", id)
	}
	return nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanProject(s scanner) (*model.Project, error) {
	var p model.Project
	if err := s.Scan(&p.ID, &p.WorkDir, &p.CreatedAt, &p.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("project not found")
		}
		return nil, fmt.Errorf("scan project: %w", err)
	}
	return &p, nil
}

func scanProjects(rows *sql.Rows) ([]*model.Project, error) {
	var projects []*model.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}
