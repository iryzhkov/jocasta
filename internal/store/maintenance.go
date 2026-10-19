package store

import (
	"context"
	"encoding/json"
	"fmt"

	"time"
)

func (s *Store) ArchiveInactive(ctx context.Context, days int) (int, error) {
	if err := requireWrite(ctx); err != nil {
		return 0, err
	}
	if days < 1 {
		return 0, Invalid("archive days must be positive")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "UPDATE state SET generation=generation WHERE singleton=1"); err != nil {
		return 0, err
	}
	cutoff := time.Now().UTC().Add(-time.Duration(days) * 24 * time.Hour).Format(time.RFC3339Nano)
	rows, err := tx.QueryContext(ctx, "SELECT d.id FROM documents d JOIN activity a ON a.id=d.id WHERE d.archived=0 AND a.last_active<? ORDER BY d.id LIMIT 100", cutoff)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, raw)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		d, _, e := resolve(ctx, tx, "", id, 0)
		if e != nil {
			return 0, e
		}
		encodedSnapshot, _ := json.Marshal(d)
		if err = s.checkBudget(int64(len(encodedSnapshot))*8 + 65536); err != nil {
			return 0, err
		}
		d.Revision++
		d.Archived = true
		d.Actor = "system:inactivity"
		d.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		d.Message = fmt.Sprintf("Automatically archived after %d inactive days", days)
		raw, _ := json.Marshal(d)
		if _, err = tx.ExecContext(ctx, "UPDATE documents SET revision=?,archived=1,updated_at=?,snapshot=? WHERE id=?", d.Revision, d.UpdatedAt, string(raw), d.ID); err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO revisions VALUES(?,?,?)", d.ID, d.Revision, string(raw)); err != nil {
			return 0, err
		}
	}
	if len(ids) > 0 {
		if _, err = tx.ExecContext(ctx, "UPDATE state SET generation=generation+1 WHERE singleton=1"); err != nil {
			return 0, err
		}
	}
	return len(ids), tx.Commit()
}
