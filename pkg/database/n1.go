package database

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var (
	ErrN1Conflict = errors.New("idempotency_conflict")
	ErrN1Budget   = errors.New("budget_exhausted")
	ErrN1State    = errors.New("invalid_execution_state")
)

// N1Store uses the application's SQLite connection, never a parallel authority.
type N1Store struct{ db *sql.DB }

func (db *sqliteDB) N1Store() *N1Store { return &N1Store{db: db.DB} }

type RunBudget struct {
	Requests         int64 `json:"requests"`
	Tokens           int64 `json:"tokens"`
	Bytes            int64 `json:"bytes"`
	ReservedRequests int64 `json:"reserved_requests"`
	ReservedTokens   int64 `json:"reserved_tokens"`
	ReservedBytes    int64 `json:"reserved_bytes"`
}
type ExecutionRun struct {
	ID             string          `json:"run_id"`
	Kind           string          `json:"kind"`
	Scope          string          `json:"-"`
	Instance       string          `json:"instance"`
	Boot           string          `json:"boot_at_accept"`
	Key            string          `json:"idempotency_key"`
	PayloadDigest  string          `json:"payload_digest"`
	Payload        json.RawMessage `json:"-"`
	Manifest       json.RawMessage `json:"manifest"`
	State          string          `json:"state"`
	Stage          string          `json:"stage"`
	Deadline       int64           `json:"deadline"`
	Budget         RunBudget       `json:"budget"`
	Cleanup        string          `json:"cleanup_state"`
	Terminal       json.RawMessage `json:"terminal"`
	FindingsRef    *int64          `json:"findings_ref"`
	FindingsDigest *string         `json:"findings_digest"`
	AnalysisState  string          `json:"analysis_state"`
	FrozenAt       *int64          `json:"frozen_at"`
	CreatedAt      int64           `json:"created_at"`
}
type ExecutionEvent struct {
	RunID           string          `json:"run_id"`
	Seq             int64           `json:"seq"`
	Type            string          `json:"event_type"`
	Payload         json.RawMessage `json:"payload"`
	Digest          string          `json:"digest"`
	CreatedAt       int64           `json:"created_at"`
	Key             string          `json:"event_key,omitempty"`
	AnalysisVersion *int            `json:"analysis_version,omitempty"`
}

func N1Digest(b []byte) string { d := sha256.Sum256(b); return hex.EncodeToString(d[:]) }
func canonicalN1(b []byte) ([]byte, error) {
	var v any
	// UseNumber avoids silently rounding integer identities.
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

const runColumns = `run_id,kind,scope,instance,boot_at_accept,idempotency_key,payload_digest,payload_json,manifest_json,state,stage,deadline,budget_json,cleanup_state,terminal_json,findings_ref,findings_digest,analysis_state,frozen_at,created_at`

type rowScanner interface{ Scan(...any) error }

func scanRun(row rowScanner) (ExecutionRun, error) {
	var r ExecutionRun
	var p, m, b, t string
	err := row.Scan(&r.ID, &r.Kind, &r.Scope, &r.Instance, &r.Boot, &r.Key, &r.PayloadDigest, &p, &m, &r.State, &r.Stage, &r.Deadline, &b, &r.Cleanup, &t, &r.FindingsRef, &r.FindingsDigest, &r.AnalysisState, &r.FrozenAt, &r.CreatedAt)
	if err != nil {
		return r, err
	}
	r.Payload = []byte(p)
	r.Manifest = []byte(m)
	r.Terminal = []byte(t)
	err = json.Unmarshal([]byte(b), &r.Budget)
	return r, err
}
func (s *N1Store) Get(ctx context.Context, id string) (ExecutionRun, error) {
	return scanRun(s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM execution_runs WHERE run_id=?`, id))
}
func (s *N1Store) Lookup(ctx context.Context, scope, name, boot, key string) (ExecutionRun, error) {
	return scanRun(s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM execution_runs WHERE scope=? AND instance=? AND boot_at_accept=? AND idempotency_key=?`, scope, name, boot, key))
}
func (s *N1Store) List(ctx context.Context, scope, name string, admin bool) ([]ExecutionRun, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+runColumns+` FROM execution_runs WHERE instance=? AND (? OR scope=?) ORDER BY created_at DESC LIMIT 256`, name, admin, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ExecutionRun{}
	for rows.Next() {
		r, e := scanRun(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s *N1Store) Accept(ctx context.Context, r ExecutionRun) (ExecutionRun, bool, error) {
	p, err := canonicalN1(r.Payload)
	if err != nil {
		return r, false, err
	}
	m, err := canonicalN1(r.Manifest)
	if err != nil {
		return r, false, err
	}
	digest := N1Digest(p)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return r, false, err
	}
	defer tx.Rollback()
	old, e := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM execution_runs WHERE scope=? AND instance=? AND boot_at_accept=? AND idempotency_key=?`, r.Scope, r.Instance, r.Boot, r.Key))
	if e == nil {
		if old.PayloadDigest != digest {
			return old, false, ErrN1Conflict
		}
		return old, true, nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return r, false, e
	}
	if r.Deadline <= time.Now().UnixMilli() || r.Budget.Requests < 0 || r.Budget.Tokens < 0 || r.Budget.Bytes < 0 || r.Budget.ReservedRequests != 0 || r.Budget.ReservedTokens != 0 || r.Budget.ReservedBytes != 0 {
		return r, false, ErrN1Budget
	}
	r.Payload = p
	r.Manifest = m
	r.PayloadDigest = digest
	r.State = "accepted"
	r.Stage = "accepted"
	r.Cleanup = "pending"
	r.AnalysisState = "awaiting"
	r.Terminal = json.RawMessage(`{}`)
	r.CreatedAt = time.Now().UnixMilli()
	budget, _ := json.Marshal(r.Budget)
	_, err = tx.ExecContext(ctx, `INSERT INTO execution_runs (`+runColumns+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, r.ID, r.Kind, r.Scope, r.Instance, r.Boot, r.Key, digest, string(p), string(m), r.State, r.Stage, r.Deadline, string(budget), r.Cleanup, string(r.Terminal), nil, nil, r.AnalysisState, nil, r.CreatedAt)
	if err != nil {
		return r, false, err
	}
	return r, false, tx.Commit()
}

const eventColumns = `run_id,seq,event_type,payload_json,digest,created_at,event_key,analysis_version`

func scanEvent(row rowScanner) (ExecutionEvent, error) {
	var e ExecutionEvent
	var p string
	var key sql.NullString
	err := row.Scan(&e.RunID, &e.Seq, &e.Type, &p, &e.Digest, &e.CreatedAt, &key, &e.AnalysisVersion)
	e.Payload = []byte(p)
	e.Key = key.String
	return e, err
}
func appendEvent(ctx context.Context, tx *sql.Tx, id, kind, key string, p []byte, version *int) (ExecutionEvent, bool, error) {
	var old ExecutionEvent
	if key != "" {
		e, err := scanEvent(tx.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM execution_events WHERE run_id=? AND event_type=? AND event_key=?`, id, kind, key))
		if err == nil {
			if e.Digest != N1Digest(p) {
				return e, false, ErrN1Conflict
			}
			return e, true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return old, false, err
		}
	}
	var seq int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0)+1 FROM execution_events WHERE run_id=?`, id).Scan(&seq); err != nil {
		return old, false, err
	}
	e := ExecutionEvent{RunID: id, Seq: seq, Type: kind, Key: key, Payload: p, Digest: N1Digest(p), CreatedAt: time.Now().UnixMilli(), AnalysisVersion: version}
	var k any
	if key != "" {
		k = key
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO execution_events (`+eventColumns+`) VALUES(?,?,?,?,?,?,?,?)`, id, seq, kind, string(p), e.Digest, e.CreatedAt, k, version)
	return e, false, err
}
func (s *N1Store) Events(ctx context.Context, id string) ([]ExecutionEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+eventColumns+` FROM execution_events WHERE run_id=? ORDER BY seq`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ExecutionEvent{}
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
func (s *N1Store) Dispatch(ctx context.Context, id, key string, payload json.RawMessage, tokens, bytes int64) (ExecutionEvent, bool, error) {
	var zero ExecutionEvent
	p, err := canonicalN1(payload)
	if err != nil {
		return zero, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, false, err
	}
	defer tx.Rollback()
	old, e := scanEvent(tx.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM execution_events WHERE run_id=? AND event_type='dispatch' AND event_key=?`, id, key))
	if e == nil {
		if old.Digest != N1Digest(p) {
			return old, false, ErrN1Conflict
		}
		return old, true, nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return zero, false, e
	}
	r, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM execution_runs WHERE run_id=?`, id))
	if err != nil {
		return zero, false, err
	}
	if (r.State != "accepted" && r.State != "running") || r.FrozenAt != nil {
		return zero, false, ErrN1State
	}
	b := r.Budget
	if time.Now().UnixMilli() >= r.Deadline || tokens <= 0 || bytes <= 0 || b.ReservedRequests >= b.Requests || tokens > b.Tokens-b.ReservedTokens || bytes > b.Bytes-b.ReservedBytes {
		return zero, false, ErrN1Budget
	}
	b.ReservedRequests++
	b.ReservedTokens += tokens
	b.ReservedBytes += bytes
	raw, _ := json.Marshal(b)
	if _, err = tx.ExecContext(ctx, `UPDATE execution_runs SET budget_json=?,state='running',stage='dispatch' WHERE run_id=?`, string(raw), id); err != nil {
		return zero, false, err
	}
	eout, _, err := appendEvent(ctx, tx, id, "dispatch", key, p, nil)
	if err != nil {
		return zero, false, err
	}
	return eout, false, tx.Commit()
}
func (s *N1Store) Append(ctx context.Context, id, kind, key string, payload json.RawMessage) (ExecutionEvent, error) {
	var zero ExecutionEvent
	p, err := canonicalN1(payload)
	if err != nil {
		return zero, err
	}
	if kind == "dispatch" || kind == "findings_v1" {
		return zero, ErrN1State
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	r, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM execution_runs WHERE run_id=?`, id))
	if err != nil {
		return zero, err
	}
	if kind == "receipt" && r.FrozenAt != nil {
		return zero, ErrN1State
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM execution_events WHERE run_id=?`, id).Scan(&count); err != nil {
		return zero, err
	}
	if count >= 4096 {
		return zero, ErrN1Budget
	}
	e, _, err := appendEvent(ctx, tx, id, kind, key, p, nil)
	if err != nil {
		return zero, err
	}
	return e, tx.Commit()
}
func (s *N1Store) Freeze(ctx context.Context, id, state, cleanup string, terminal json.RawMessage) error {
	if cleanup != "released" && cleanup != "unknown" && cleanup != "failed" {
		return ErrN1State
	}
	switch state {
	case "succeeded", "partial", "failed", "cancelled", "deadline_exceeded", "interrupted":
	default:
		return ErrN1State
	}
	if state == "succeeded" && cleanup != "released" {
		return ErrN1State
	}
	p, err := canonicalN1(terminal)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE execution_runs SET state=?,stage='finished',cleanup_state=?,terminal_json=?,frozen_at=? WHERE run_id=? AND frozen_at IS NULL`, state, cleanup, string(p), time.Now().UnixMilli(), id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		return ErrN1State
	}
	if _, _, err = appendEvent(ctx, tx, id, "terminal", "", p, nil); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *N1Store) Finalize(ctx context.Context, id string, version int, payload json.RawMessage) (ExecutionEvent, bool, error) {
	var zero ExecutionEvent
	p, err := canonicalN1(payload)
	if err != nil {
		return zero, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, false, err
	}
	defer tx.Rollback()
	old, err := scanEvent(tx.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM execution_events WHERE run_id=? AND analysis_version=?`, id, version))
	if err == nil {
		if old.Digest != N1Digest(p) {
			return old, false, ErrN1Conflict
		}
		return old, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return zero, false, err
	}
	r, err := scanRun(tx.QueryRowContext(ctx, `SELECT `+runColumns+` FROM execution_runs WHERE run_id=?`, id))
	if err != nil {
		return zero, false, err
	}
	if version < 1 || r.FrozenAt == nil || time.Now().UnixMilli() > *r.FrozenAt+int64(24*time.Hour/time.Millisecond) {
		return zero, false, ErrN1State
	}
	var max int
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(analysis_version),0) FROM execution_events WHERE run_id=?`, id).Scan(&max); err != nil {
		return zero, false, err
	}
	if version != max+1 {
		return zero, false, ErrN1Conflict
	}
	e, _, err := appendEvent(ctx, tx, id, "findings_v1", fmt.Sprint(version), p, &version)
	if err != nil {
		return zero, false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE execution_runs SET findings_ref=?,findings_digest=?,analysis_state='finalized' WHERE run_id=?`, e.Seq, e.Digest, id); err != nil {
		return zero, false, err
	}
	return e, false, tx.Commit()
}
func (s *N1Store) Interrupt(ctx context.Context, boot string) error {
	// Atomic mark only. No manifest replay, loading or process adoption.
	_, err := s.db.ExecContext(ctx, `UPDATE execution_runs SET state='interrupted',stage='restart_recovery',cleanup_state='unknown',frozen_at=? WHERE boot_at_accept<>? AND frozen_at IS NULL`, time.Now().UnixMilli(), boot)
	return err
}
