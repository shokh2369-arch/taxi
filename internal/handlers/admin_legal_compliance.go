package handlers

import (
	"context"
	"database/sql"
	"fmt"

	"taxi-mvp/internal/legal"
)

// loadActiveDocumentVersions returns active published version per document_type (source of truth for compliance).
func loadActiveDocumentVersions(ctx context.Context, db *sql.DB) (map[string]int, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT document_type, version FROM legal_documents WHERE is_active = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int)
	for rows.Next() {
		var dt string
		var ver int
		if err := rows.Scan(&dt, &ver); err != nil {
			return nil, err
		}
		out[dt] = ver
	}
	return out, rows.Err()
}

func requiredDocTypesForRole(role string) []string {
	switch role {
	case "driver", "drivers":
		return []string{legal.DocDriverTerms, legal.DocPrivacyPolicy}
	default:
		return []string{legal.DocUserTerms, legal.DocPrivacyPolicy}
	}
}

// userAcceptanceKey: userID -> document_type -> accepted version (from legal_acceptances only).
type userAcceptanceIndex map[int64]map[string]int

func loadAcceptanceIndex(ctx context.Context, db *sql.DB, userIDs []int64) (userAcceptanceIndex, error) {
	idx := make(userAcceptanceIndex)
	if len(userIDs) == 0 {
		return idx, nil
	}
	ph := ""
	args := make([]interface{}, 0, len(userIDs))
	for i, id := range userIDs {
		if i > 0 {
			ph += ","
		}
		ph += "?"
		args = append(args, id)
	}
	q := fmt.Sprintf(`SELECT user_id, document_type, version FROM legal_acceptances WHERE user_id IN (%s)`, ph)
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var uid int64
		var dt string
		var ver int
		if err := rows.Scan(&uid, &dt, &ver); err != nil {
			return nil, err
		}
		if idx[uid] == nil {
			idx[uid] = make(map[string]int)
		}
		idx[uid][dt] = ver
	}
	return idx, rows.Err()
}

// loadAcceptanceIndexForDriversAndRiders loads all legal_acceptances rows for current drivers and riders (admin compliance).
func loadAcceptanceIndexForDriversAndRiders(ctx context.Context, db *sql.DB) (userAcceptanceIndex, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT la.user_id, la.document_type, la.version
		FROM legal_acceptances la
		WHERE la.user_id IN (
			SELECT user_id FROM drivers
			UNION
			SELECT id FROM users WHERE role = 'rider'
		)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	idx := make(userAcceptanceIndex)
	for rows.Next() {
		var uid int64
		var dt string
		var ver int
		if err := rows.Scan(&uid, &dt, &ver); err != nil {
			return nil, err
		}
		if idx[uid] == nil {
			idx[uid] = make(map[string]int)
		}
		idx[uid][dt] = ver
	}
	return idx, rows.Err()
}

// missingDocsForUser returns document_type values the user still needs at currently active versions.
func missingDocsForUser(userID int64, role string, active map[string]int, acc userAcceptanceIndex) []string {
	req := requiredDocTypesForRole(role)
	var missing []string
	byType := acc[userID]
	if byType == nil {
		byType = map[string]int{}
	}
	for _, dt := range req {
		want, published := active[dt]
		if !published {
			missing = append(missing, dt)
			continue
		}
		got, ok := byType[dt]
		if !ok || got != want {
			missing = append(missing, dt)
		}
	}
	return missing
}

func versionLabel(v int) string {
	if v <= 0 {
		return ""
	}
	return fmt.Sprintf("v%d", v)
}
