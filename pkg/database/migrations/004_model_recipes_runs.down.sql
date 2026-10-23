-- Administrative schema teardown only; not a production rollback procedure.
DROP TRIGGER recipe_immutable_delete;
DROP TRIGGER recipe_version_immutable;
DROP TRIGGER execution_run_immutable_delete;
DROP TRIGGER execution_terminal_immutable;
DROP TRIGGER execution_manifest_immutable;
DROP TRIGGER execution_event_immutable_delete;
DROP TRIGGER execution_event_immutable_update;
DROP TABLE api_key_capabilities;
-- Break the optional findings reference before dropping the circular tables.
UPDATE execution_runs SET findings_ref = NULL;
DROP TABLE execution_events;
DROP TABLE execution_runs;
DROP TABLE recipes;
DROP TABLE model_artifacts;
