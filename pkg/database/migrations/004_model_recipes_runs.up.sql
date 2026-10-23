-- N1 evidence shares the existing SQLite authority. No execution is replayed.
CREATE TABLE model_artifacts (
    artifact_id TEXT PRIMARY KEY,
    root_id TEXT NOT NULL,
    relative_main TEXT NOT NULL,
    ordered_members_json TEXT NOT NULL,
    projector_id TEXT,
    metadata_json TEXT NOT NULL,
    file_state TEXT NOT NULL,
    manifest_digest TEXT NOT NULL,
    identity_level TEXT NOT NULL,
    stat_cache TEXT NOT NULL,
    last_scan_id TEXT NOT NULL,
    UNIQUE(root_id, relative_main)
);
CREATE TABLE recipes (
    instance TEXT NOT NULL,
    recipe_id TEXT NOT NULL,
    version INTEGER NOT NULL CHECK(version > 0),
    recipe_json TEXT NOT NULL,
    recipe_digest TEXT NOT NULL,
    artifact_id TEXT,
    artifact_manifest_digest TEXT NOT NULL,
    publication_state TEXT NOT NULL DEFAULT 'draft' CHECK(publication_state IN ('draft','published','withdrawn')),
    publication_audit_json TEXT NOT NULL DEFAULT '[]',
    created_by TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY(recipe_id, version)
);
CREATE INDEX recipes_instance ON recipes(instance);
CREATE TABLE execution_runs (
    run_id TEXT PRIMARY KEY,
    kind TEXT NOT NULL CHECK(kind IN ('evaluation','switch')),
    scope TEXT NOT NULL,
    instance TEXT NOT NULL,
    boot_at_accept TEXT NOT NULL,
    idempotency_key TEXT NOT NULL,
    payload_digest TEXT NOT NULL,
    payload_json TEXT NOT NULL,
    manifest_json TEXT NOT NULL,
    state TEXT NOT NULL,
    stage TEXT NOT NULL,
    deadline INTEGER NOT NULL,
    budget_json TEXT NOT NULL,
    cleanup_state TEXT NOT NULL,
    before_refs TEXT NOT NULL DEFAULT '{}',
    after_refs TEXT NOT NULL DEFAULT '{}',
    terminal_json TEXT NOT NULL DEFAULT '{}',
    findings_ref INTEGER,
    findings_digest TEXT,
    analysis_state TEXT NOT NULL DEFAULT 'awaiting',
    frozen_at INTEGER,
    created_at INTEGER NOT NULL DEFAULT 0,
    UNIQUE(scope, instance, boot_at_accept, idempotency_key),
    FOREIGN KEY(run_id, findings_ref) REFERENCES execution_events(run_id, seq)
);
CREATE TABLE execution_events (
    run_id TEXT NOT NULL REFERENCES execution_runs(run_id),
    seq INTEGER NOT NULL CHECK(seq > 0),
    event_type TEXT NOT NULL,
    payload_json TEXT NOT NULL,
    digest TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    event_key TEXT,
    analysis_version INTEGER,
    PRIMARY KEY(run_id, seq),
    UNIQUE(run_id, event_type, event_key),
    UNIQUE(run_id, analysis_version)
);
CREATE TABLE api_key_capabilities (
    key_id INTEGER NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    instance_id INTEGER NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
    capability TEXT NOT NULL CHECK(capability IN ('discover','evaluate','recipe_write','recipe_publish','service_use','inference','observe')),
    PRIMARY KEY(key_id, instance_id, capability)
);
CREATE TRIGGER execution_event_immutable_update BEFORE UPDATE ON execution_events BEGIN SELECT RAISE(ABORT, 'immutable execution event'); END;
CREATE TRIGGER execution_event_immutable_delete BEFORE DELETE ON execution_events BEGIN SELECT RAISE(ABORT, 'immutable execution event'); END;
CREATE TRIGGER execution_manifest_immutable BEFORE UPDATE ON execution_runs
WHEN NEW.run_id IS NOT OLD.run_id OR NEW.kind IS NOT OLD.kind OR NEW.scope IS NOT OLD.scope OR NEW.instance IS NOT OLD.instance OR NEW.boot_at_accept IS NOT OLD.boot_at_accept OR NEW.idempotency_key IS NOT OLD.idempotency_key OR NEW.payload_digest IS NOT OLD.payload_digest OR NEW.payload_json IS NOT OLD.payload_json OR NEW.manifest_json IS NOT OLD.manifest_json OR NEW.deadline IS NOT OLD.deadline
BEGIN SELECT RAISE(ABORT, 'immutable execution manifest'); END;
CREATE TRIGGER execution_terminal_immutable BEFORE UPDATE ON execution_runs
WHEN OLD.frozen_at IS NOT NULL AND (NEW.state IS NOT OLD.state OR NEW.stage IS NOT OLD.stage OR NEW.cleanup_state IS NOT OLD.cleanup_state OR NEW.terminal_json IS NOT OLD.terminal_json OR NEW.frozen_at IS NOT OLD.frozen_at OR NEW.budget_json IS NOT OLD.budget_json)
BEGIN SELECT RAISE(ABORT, 'immutable execution terminal'); END;
CREATE TRIGGER execution_run_immutable_delete BEFORE DELETE ON execution_runs BEGIN SELECT RAISE(ABORT, 'immutable execution run'); END;
CREATE TRIGGER recipe_version_immutable BEFORE UPDATE ON recipes
WHEN NEW.instance IS NOT OLD.instance OR NEW.recipe_id IS NOT OLD.recipe_id OR NEW.version IS NOT OLD.version OR NEW.recipe_json IS NOT OLD.recipe_json OR NEW.recipe_digest IS NOT OLD.recipe_digest OR NEW.artifact_id IS NOT OLD.artifact_id OR NEW.artifact_manifest_digest IS NOT OLD.artifact_manifest_digest OR NEW.created_by IS NOT OLD.created_by OR NEW.created_at IS NOT OLD.created_at
BEGIN SELECT RAISE(ABORT, 'immutable recipe version'); END;
CREATE TRIGGER recipe_immutable_delete BEFORE DELETE ON recipes BEGIN SELECT RAISE(ABORT, 'immutable recipe version'); END;
