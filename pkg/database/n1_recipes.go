package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"llamactl/pkg/auth"
	"llamactl/pkg/models"
	"time"
)

type RecipeRecord struct {
	Instance       string          `json:"instance"`
	ID             string          `json:"recipe_id"`
	Version        int             `json:"version"`
	Document       json.RawMessage `json:"recipe"`
	Digest         string          `json:"recipe_digest"`
	ArtifactID     string          `json:"artifact_id"`
	ArtifactDigest string          `json:"artifact_manifest_digest"`
	Publication    string          `json:"publication_state"`
	CreatedBy      string          `json:"-"`
	CreatedAt      int64           `json:"created_at"`
}

const recipeColumns = `instance,recipe_id,version,recipe_json,recipe_digest,COALESCE(artifact_id,''),artifact_manifest_digest,publication_state,created_by,created_at`

func scanRecipe(row rowScanner) (RecipeRecord, error) {
	var r RecipeRecord
	var b string
	err := row.Scan(&r.Instance, &r.ID, &r.Version, &b, &r.Digest, &r.ArtifactID, &r.ArtifactDigest, &r.Publication, &r.CreatedBy, &r.CreatedAt)
	r.Document = []byte(b)
	return r, err
}
func (s *N1Store) GetRecipe(ctx context.Context, name, id string, version int) (RecipeRecord, error) {
	return scanRecipe(s.db.QueryRowContext(ctx, `SELECT `+recipeColumns+` FROM recipes WHERE instance=? AND recipe_id=? AND version=?`, name, id, version))
}
func (s *N1Store) Recipes(ctx context.Context, name string) ([]RecipeRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+recipeColumns+` FROM recipes WHERE instance=? ORDER BY recipe_id,version LIMIT 1024`, name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RecipeRecord{}
	for rows.Next() {
		r, err := scanRecipe(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s *N1Store) PutRecipe(ctx context.Context, r RecipeRecord) (RecipeRecord, bool, error) {
	p, err := canonicalN1(r.Document)
	if err != nil {
		return r, false, err
	}
	r.Document = p
	r.Digest = N1Digest(p)
	r.Publication = "draft"
	r.CreatedAt = time.Now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return r, false, err
	}
	defer tx.Rollback()
	old, e := scanRecipe(tx.QueryRowContext(ctx, `SELECT `+recipeColumns+` FROM recipes WHERE recipe_id=? AND version=?`, r.ID, r.Version))
	if e == nil {
		if old.Instance != r.Instance || old.Digest != r.Digest || old.ArtifactDigest != r.ArtifactDigest {
			return old, false, ErrN1Conflict
		}
		return old, true, nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return r, false, e
	}
	var max int
	var scope sql.NullString
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0),MIN(instance) FROM recipes WHERE recipe_id=?`, r.ID).Scan(&max, &scope); err != nil {
		return r, false, err
	}
	if r.Version != max+1 || (scope.Valid && scope.String != r.Instance) {
		return r, false, ErrN1Conflict
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO recipes(instance,recipe_id,version,recipe_json,recipe_digest,artifact_id,artifact_manifest_digest,publication_state,created_by,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, r.Instance, r.ID, r.Version, string(p), r.Digest, r.ArtifactID, r.ArtifactDigest, r.Publication, r.CreatedBy, r.CreatedAt)
	if err != nil {
		return r, false, err
	}
	return r, false, tx.Commit()
}
func (s *N1Store) PublishRecipe(ctx context.Context, name, id string, version int, scope, state string) error {
	if state != "published" && state != "withdrawn" {
		return ErrN1State
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var raw string
	if err = tx.QueryRowContext(ctx, `SELECT publication_audit_json FROM recipes WHERE instance=? AND recipe_id=? AND version=?`, name, id, version).Scan(&raw); err != nil {
		return err
	}
	var audit []map[string]any
	if err = json.Unmarshal([]byte(raw), &audit); err != nil {
		return err
	}
	if len(audit) >= 128 {
		return ErrN1Budget
	}
	audit = append(audit, map[string]any{"scope": scope, "state": state, "at": time.Now().UnixMilli()})
	b, _ := json.Marshal(audit)
	if _, err = tx.ExecContext(ctx, `UPDATE recipes SET publication_state=?,publication_audit_json=? WHERE instance=? AND recipe_id=? AND version=?`, state, string(b), name, id, version); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *N1Store) SaveArtifact(ctx context.Context, a models.Artifact) (models.Artifact, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return a, err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRowContext(ctx, `SELECT artifact_id FROM model_artifacts WHERE root_id=? AND relative_main=?`, a.RootID, a.Main).Scan(&id)
	if err == nil {
		a.ID = id
	} else if !errors.Is(err, sql.ErrNoRows) {
		return a, err
	}
	members, _ := json.Marshal(a.Members)
	meta, _ := json.Marshal(a.Metadata)
	_, err = tx.ExecContext(ctx, `INSERT INTO model_artifacts(artifact_id,root_id,relative_main,ordered_members_json,metadata_json,file_state,manifest_digest,identity_level,stat_cache,last_scan_id) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(root_id,relative_main) DO UPDATE SET ordered_members_json=excluded.ordered_members_json,metadata_json=excluded.metadata_json,file_state=excluded.file_state,manifest_digest=excluded.manifest_digest,identity_level=excluded.identity_level,stat_cache=excluded.stat_cache,last_scan_id=excluded.last_scan_id`, a.ID, a.RootID, a.Main, string(members), string(meta), a.State, a.ManifestDigest, a.IdentityLevel, string(members), a.ScanID)
	if err != nil {
		return a, err
	}
	return a, tx.Commit()
}
func (s *N1Store) Artifact(ctx context.Context, id string) (models.Artifact, error) {
	var a models.Artifact
	var members, meta string
	err := s.db.QueryRowContext(ctx, `SELECT artifact_id,root_id,relative_main,ordered_members_json,metadata_json,file_state,manifest_digest,identity_level,last_scan_id FROM model_artifacts WHERE artifact_id=?`, id).Scan(&a.ID, &a.RootID, &a.Main, &members, &meta, &a.State, &a.ManifestDigest, &a.IdentityLevel, &a.ScanID)
	if err != nil {
		return a, err
	}
	if err = json.Unmarshal([]byte(members), &a.Members); err != nil {
		return a, err
	}
	err = json.Unmarshal([]byte(meta), &a.Metadata)
	return a, err
}
func (s *N1Store) ArtifactCache(ctx context.Context) ([]models.Artifact, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT artifact_id FROM model_artifacts LIMIT 10000`)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := []models.Artifact{}
	for _, id := range ids {
		a, e := s.Artifact(ctx, id)
		if e != nil {
			return nil, e
		}
		out = append(out, a)
	}
	return out, nil
}
func (s *N1Store) ArtifactByDigest(ctx context.Context, digest string) (models.Artifact, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT artifact_id FROM model_artifacts WHERE manifest_digest=? AND identity_level='verified' ORDER BY artifact_id LIMIT 1`, digest).Scan(&id)
	if err != nil {
		return models.Artifact{}, err
	}
	return s.Artifact(ctx, id)
}
func (s *N1Store) HasCapability(ctx context.Context, keyID, instanceID int, capability string) (bool, error) {
	if !auth.ValidCapability(capability) {
		return false, fmt.Errorf("unknown capability")
	}
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM api_key_capabilities c JOIN api_keys k ON k.id=c.key_id WHERE c.key_id=? AND c.instance_id=? AND c.capability=? AND (k.expires_at IS NULL OR k.expires_at>?))`, keyID, instanceID, capability, time.Now().Unix()).Scan(&exists)
	return exists, err
}
func (s *N1Store) SetCapabilities(ctx context.Context, keyID, instanceID int, caps []string) error {
	seen := map[string]bool{}
	for _, c := range caps {
		if !auth.ValidCapability(c) || seen[c] {
			return fmt.Errorf("invalid capability")
		}
		seen[c] = true
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM api_key_capabilities WHERE key_id=? AND instance_id=?`, keyID, instanceID); err != nil {
		return err
	}
	for _, c := range caps {
		if _, err = tx.ExecContext(ctx, `INSERT INTO api_key_capabilities(key_id,instance_id,capability) VALUES(?,?,?)`, keyID, instanceID, c); err != nil {
			return err
		}
	}
	return tx.Commit()
}
