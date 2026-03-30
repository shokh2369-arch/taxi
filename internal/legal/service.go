package legal

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Service reads active legal documents and records acceptances (active version only).
type Service struct {
	db *sql.DB
}

func NewService(db *sql.DB) *Service {
	return &Service{db: db}
}

// ActiveDocument returns content and version for the active row of document_type.
func (s *Service) ActiveDocument(ctx context.Context, documentType string) (version int, content string, err error) {
	err = s.db.QueryRowContext(ctx, `
		SELECT version, content FROM legal_documents
		WHERE document_type = ?1 AND is_active = 1 LIMIT 1`,
		documentType).Scan(&version, &content)
	return
}

// ActiveDocuments returns all active documents for the given types (in stable order).
func (s *Service) ActiveDocuments(ctx context.Context, types []string) (map[string]struct{ Version int; Content string }, error) {
	out := make(map[string]struct{ Version int; Content string })
	if len(types) == 0 {
		return out, nil
	}
	ph := strings.Repeat("?,", len(types))
	ph = ph[:len(ph)-1]
	q := fmt.Sprintf(`SELECT document_type, version, content FROM legal_documents WHERE is_active = 1 AND document_type IN (%s)`, ph)
	args := make([]interface{}, len(types))
	for i, t := range types {
		args[i] = t
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var dt string
		var ver int
		var c string
		if err := rows.Scan(&dt, &ver, &c); err != nil {
			return nil, err
		}
		out[dt] = struct{ Version int; Content string }{Version: ver, Content: c}
	}
	return out, rows.Err()
}

func (s *Service) countActiveMatched(ctx context.Context, userID int64, types []string) (int, error) {
	if len(types) == 0 {
		return 0, nil
	}
	ph := strings.Repeat("?,", len(types))
	ph = ph[:len(ph)-1]
	q := fmt.Sprintf(`
		SELECT COUNT(*) FROM legal_acceptances la
		INNER JOIN legal_documents ld ON ld.document_type = la.document_type AND ld.version = la.version AND ld.is_active = 1
		WHERE la.user_id = ?1 AND la.document_type IN (%s)`, ph)
	args := make([]interface{}, 0, 1+len(types))
	args = append(args, userID)
	for _, t := range types {
		args = append(args, t)
	}
	var n int
	err := s.db.QueryRowContext(ctx, q, args...).Scan(&n)
	return n, err
}

// RiderHasActiveLegal returns true when user_terms and user privacy policy are accepted at currently active versions.
func (s *Service) RiderHasActiveLegal(ctx context.Context, userID int64) bool {
	n, err := s.countActiveMatched(ctx, userID, []string{DocUserTerms, DocPrivacyPolicyUser})
	return err == nil && n == RiderDocTypes
}

// DriverHasActiveLegal returns true when driver_terms and driver privacy policy are accepted at active versions.
func (s *Service) DriverHasActiveLegal(ctx context.Context, userID int64) bool {
	n, err := s.countActiveMatched(ctx, userID, []string{DocDriverTerms, DocPrivacyPolicyDriver})
	return err == nil && n == DriverDocTypes
}

// DriverHasDriverTermsOnly returns true when driver_terms active version is accepted (registration / pre-approval).
func (s *Service) DriverHasDriverTermsOnly(ctx context.Context, userID int64) bool {
	n, err := s.countActiveMatched(ctx, userID, []string{DocDriverTerms})
	return err == nil && n == 1
}

// AcceptActiveForTypes records acceptance of the current active version for each type (ignores any client-supplied version).
func (s *Service) AcceptActiveForTypes(ctx context.Context, userID int64, types []string, clientIP, userAgent string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for _, dt := range types {
		var ver int
		err := tx.QueryRowContext(ctx, `SELECT version FROM legal_documents WHERE document_type = ?1 AND is_active = 1`, dt).Scan(&ver)
		if err != nil {
			return fmt.Errorf("no active document for %s: %w", dt, err)
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO legal_acceptances (user_id, document_type, version, client_ip, user_agent)
			VALUES (?1, ?2, ?3, ?4, ?5)
			ON CONFLICT(user_id, document_type) DO UPDATE SET
				version = excluded.version,
				accepted_at = datetime('now'),
				client_ip = excluded.client_ip,
				user_agent = excluded.user_agent`,
			userID, dt, ver, nullIfEmpty(clientIP), nullIfEmpty(userAgent))
		if err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	return s.syncLegacyTermsFlags(ctx, userID)
}

func nullIfEmpty(s string) interface{} {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func (s *Service) syncLegacyTermsFlags(ctx context.Context, userID int64) error {
	var role string
	if err := s.db.QueryRowContext(ctx, `SELECT role FROM users WHERE id = ?1`, userID).Scan(&role); err != nil {
		return err
	}
	riderOK := s.RiderHasActiveLegal(ctx, userID)
	if riderOK {
		_, _ = s.db.ExecContext(ctx, `UPDATE users SET terms_accepted = 1 WHERE id = ?1`, userID)
	} else {
		_, _ = s.db.ExecContext(ctx, `UPDATE users SET terms_accepted = 0 WHERE id = ?1`, userID)
	}
	if role == "driver" {
		if s.DriverHasActiveLegal(ctx, userID) {
			_, _ = s.db.ExecContext(ctx, `UPDATE drivers SET terms_accepted = 1 WHERE user_id = ?1`, userID)
		} else {
			_, _ = s.db.ExecContext(ctx, `UPDATE drivers SET terms_accepted = 0 WHERE user_id = ?1`, userID)
		}
	}
	return nil
}

// SetPendingResume stores a single pending resume row for the user.
func (s *Service) SetPendingResume(ctx context.Context, userID int64, kind, payload string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO legal_pending_resume (user_id, kind, payload) VALUES (?1, ?2, ?3)
		ON CONFLICT(user_id) DO UPDATE SET kind = excluded.kind, payload = excluded.payload, created_at = datetime('now')`,
		userID, kind, payload)
	return err
}

// TakePendingResume returns and deletes the pending resume row, if any.
func (s *Service) TakePendingResume(ctx context.Context, userID int64) (kind, payload string, ok bool) {
	var k, p sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT kind, payload FROM legal_pending_resume WHERE user_id = ?1`, userID).Scan(&k, &p)
	if err != nil {
		return "", "", false
	}
	_, _ = s.db.ExecContext(ctx, `DELETE FROM legal_pending_resume WHERE user_id = ?1`, userID)
	if !k.Valid {
		return "", "", false
	}
	pl := ""
	if p.Valid {
		pl = p.String
	}
	return strings.TrimSpace(k.String), pl, true
}
