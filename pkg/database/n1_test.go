package database

import (
	"context"
	"testing"
)

func TestN1MigrationAndImmutableLedger(t *testing.T) {
	db := newTestDB(t)
	for _, table := range []string{"model_artifacts", "recipes", "execution_runs", "execution_events", "api_key_capabilities"} {
		var name string
		if err := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name); err != nil {
			t.Fatalf("missing N1 table %s: %v", table, err)
		}
	}
	if err := RunMigrations(db); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, err := db.ExecContext(ctx, `INSERT INTO execution_runs(run_id,kind,scope,instance,boot_at_accept,idempotency_key,payload_digest,payload_json,manifest_json,state,stage,deadline,budget_json,cleanup_state) VALUES('r','evaluation','s','i','b','k','d','{}','{}','accepted','accepted',1,'{}','pending')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO execution_events(run_id,seq,event_type,payload_json,digest,created_at) VALUES('r',1,'dispatch','{}','d',1)`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE execution_events SET payload_json='{"changed":true}' WHERE run_id='r'`); err == nil {
		t.Fatal("immutable event changed")
	}
	if _, err = db.Exec(`UPDATE execution_runs SET manifest_json='{"changed":true}' WHERE run_id='r'`); err == nil {
		t.Fatal("immutable manifest changed")
	}
}
