package database

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

func TestN1MigrationFrom003DirtyAndRecovery(t *testing.T) {
	db, err := Open(&Config{Path: ":memory:", MaxOpenConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, name := range []string{"001_initial_schema", "002_key_permission_flags", "003_scoped_controller"} {
		b, err := migrationFiles.ReadFile("migrations/" + name + ".up.sql")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.Exec(string(b)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Exec(`CREATE TABLE schema_migrations(version uint64 NOT NULL PRIMARY KEY,dirty bool NOT NULL); INSERT INTO schema_migrations VALUES(3,1)`); err != nil {
		t.Fatal(err)
	}
	if err = RunMigrations(db); err == nil {
		t.Fatal("dirty database migrated")
	}
	if _, err = db.Exec(`UPDATE schema_migrations SET dirty=0`); err != nil {
		t.Fatal(err)
	}
	if err = RunMigrations(db); err != nil {
		t.Fatal(err)
	}
	if err = RunMigrations(db); err != nil {
		t.Fatal(err)
	}
	var version int
	if err = db.QueryRow(`SELECT version FROM schema_migrations`).Scan(&version); err != nil || version != 4 {
		t.Fatal(version, err)
	}
}
func TestN1TerminalFactsCannotBeWashedGreen(t *testing.T) {
	db := newTestDB(t)
	s := db.N1Store()
	ctx := context.Background()
	r := ExecutionRun{ID: "immutable", Kind: "evaluation", Scope: "s", Instance: "i", Boot: "b", Key: "k", Payload: json.RawMessage(`{}`), Manifest: json.RawMessage(`{}`), Deadline: time.Now().Add(time.Minute).UnixMilli()}
	if _, _, err := s.Accept(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.Freeze(ctx, r.ID, "failed", "unknown", json.RawMessage(`{"failure":"synthetic"}`)); err != nil {
		t.Fatal(err)
	}
	for _, assignment := range []string{`state='succeeded'`, `cleanup_state='released'`, `terminal_json='{}'`, `frozen_at=NULL`, `budget_json='{}'`} {
		if _, err := db.Exec(fmt.Sprintf(`UPDATE execution_runs SET %s WHERE run_id='immutable'`, assignment)); err == nil {
			t.Fatalf("terminal changed: %s", assignment)
		}
	}
}
