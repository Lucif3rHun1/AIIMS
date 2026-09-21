// Package appointments is a net-new L3 subsystem: it persists confirmed-appointment
// events emitted by the OnStatus sink in pkg/bot and exposes them via a tiny
// internal HTTP server (127.0.0.1:8080, reverse-proxy at the EasyPanel boundary).
//
// PHI rules: only display fields leave the host. health_id / ABHA / phone are
// used solely as the dedup primary key inside SQLite and never reach the
// landing page or /api/appointments response.
package appointments

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type Record struct {
	HealthID       string
	TargetDateIST  string // "YYYY-MM-DD"
	HipID          string
	PatientName    string
	HipName        string
	TokenNumber    string
	ConfirmedAtUTC time.Time
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS confirmations (
			health_id TEXT NOT NULL,
			target_date_ist TEXT NOT NULL,
			hip_id TEXT NOT NULL,
			patient_name TEXT NOT NULL DEFAULT '',
			hip_name TEXT NOT NULL DEFAULT '',
			token_number TEXT NOT NULL DEFAULT '',
			confirmed_at_utc TEXT NOT NULL,
			PRIMARY KEY (health_id, target_date_ist, hip_id)
		)
	`)
	return err
}

// Upsert writes a Record. On conflict (same dedup key), updates token/patient/hip/confirmed_at.
func (s *Store) Upsert(ctx context.Context, r Record) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO confirmations (health_id, target_date_ist, hip_id, patient_name, hip_name, token_number, confirmed_at_utc)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(health_id, target_date_ist, hip_id) DO UPDATE SET
			patient_name = excluded.patient_name,
			hip_name = excluded.hip_name,
			token_number = excluded.token_number,
			confirmed_at_utc = excluded.confirmed_at_utc
	`, r.HealthID, r.TargetDateIST, r.HipID, r.PatientName, r.HipName, r.TokenNumber, r.ConfirmedAtUTC.UTC().Format(time.RFC3339))
	return err
}

// LiveToday returns all confirmations whose target_date_ist matches the given IST calendar day.
func (s *Store) LiveToday(ctx context.Context, targetDateIST string) ([]Record, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT patient_name, hip_name, token_number, confirmed_at_utc
		FROM confirmations
		WHERE target_date_ist = ?
		ORDER BY confirmed_at_utc DESC
	`, targetDateIST)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		var r Record
		var confirmedAt string
		if err := rows.Scan(&r.PatientName, &r.HipName, &r.TokenNumber, &confirmedAt); err != nil {
			return nil, err
		}
		r.ConfirmedAtUTC, _ = time.Parse(time.RFC3339, confirmedAt)
		r.TargetDateIST = targetDateIST
		out = append(out, r)
	}
	return out, rows.Err()
}

// Ping returns nil if the DB is reachable. Used by /healthz.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
