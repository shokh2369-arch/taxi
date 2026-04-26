package repositories

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"taxi-mvp/internal/models"
)

// PlaceRepo persists admin-managed places.
type PlaceRepo struct {
	db *sql.DB
}

func NewPlaceRepo(db *sql.DB) *PlaceRepo {
	return &PlaceRepo{db: db}
}

func (r *PlaceRepo) Create(ctx context.Context, name string, lat, lng float64) (int64, error) {
	if r == nil || r.db == nil {
		return 0, fmt.Errorf("place repo unavailable")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, fmt.Errorf("place name required")
	}
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO places (name, lat, lng, created_at, updated_at)
		VALUES (?1, ?2, ?3, datetime('now'), datetime('now'))`,
		name, lat, lng)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	return id, nil
}

func (r *PlaceRepo) Delete(ctx context.Context, id int64) error {
	if r == nil || r.db == nil {
		return fmt.Errorf("place repo unavailable")
	}
	_, err := r.db.ExecContext(ctx, `DELETE FROM places WHERE id = ?1`, id)
	return err
}

func (r *PlaceRepo) List(ctx context.Context) ([]models.Place, error) {
	if r == nil || r.db == nil {
		return nil, fmt.Errorf("place repo unavailable")
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, name, lat, lng, created_at, updated_at
		FROM places
		ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.Place
	for rows.Next() {
		var p models.Place
		if err := rows.Scan(&p.ID, &p.Name, &p.Lat, &p.Lng, &p.CreatedAt, &p.UpdatedAt); err != nil {
			continue
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

