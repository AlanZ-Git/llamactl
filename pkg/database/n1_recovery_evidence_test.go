package database

import (
	"context"
	"encoding/json"
	"github.com/mattn/go-sqlite3"
	"path/filepath"
	"testing"
)

func TestN1Migration004InjectedFailureAndSnapshotRecovery(t *testing.T) {
	db, err := Open(&Config{Path: filepath.Join(t.TempDir(), "failed.db"), MaxOpenConnections: 1})
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
	if _, err = db.Exec(`CREATE TABLE schema_migrations(version uint64 NOT NULL PRIMARY KEY,dirty bool NOT NULL); INSERT INTO schema_migrations VALUES(3,0)`); err != nil {
		t.Fatal(err)
	}
	recovered := filepath.Join(t.TempDir(), "consistent-003.db")
	if _, err = db.Exec(`VACUUM INTO ?`, recovered); err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = conn.Raw(func(v any) error {
		v.(*sqlite3.SQLiteConn).RegisterAuthorizer(func(op int, a, b, c string) int {
			if op == sqlite3.SQLITE_CREATE_TABLE && a == "execution_events" {
				return sqlite3.SQLITE_DENY
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	})
	conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	err = RunMigrations(db)
	if err == nil {
		t.Fatal("injected migration failure was ignored")
	}
	t.Logf("actual injected failure: %v", err)
	var version int
	var dirty bool
	if err = db.QueryRow(`SELECT version,dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil || version != 4 || !dirty {
		t.Fatalf("dirty004 missing: %d %v %v", version, dirty, err)
	}
	if err = RunMigrations(db); err == nil {
		t.Fatal("dirty004 restarted without recovery")
	} else {
		t.Logf("dirty restart refused: %v", err)
	}
	// Recover the isolated consistent preimage, not by marking the failed DB clean.
	restored, err := Open(&Config{Path: recovered})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err = RunMigrations(restored); err != nil {
		t.Fatal(err)
	}
	if err = RunMigrations(restored); err != nil {
		t.Fatal(err)
	}
	if err = restored.QueryRow(`SELECT version,dirty FROM schema_migrations`).Scan(&version, &dirty); err != nil || version != 4 || dirty {
		t.Fatal(version, dirty, err)
	}
}

func TestN1MultipleRecipesSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recipes.db")
	db, err := Open(&Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	if err = RunMigrations(db); err != nil {
		t.Fatal(err)
	}
	docs := map[string]json.RawMessage{"one": json.RawMessage(`{"description":"one","deployment":{"context":null,"gpu_layers":0,"flash_attention":false}}`), "two": json.RawMessage(`{"description":"two","deployment":{"context":4096}}`)}
	for id, doc := range docs {
		if _, _, err = db.N1Store().PutRecipe(context.Background(), RecipeRecord{Instance: "i", ID: id, Version: 1, Document: doc, ArtifactDigest: "synthetic"}); err != nil {
			t.Fatal(err)
		}
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(&Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = RunMigrations(db); err != nil {
		t.Fatal(err)
	}
	rows, err := db.N1Store().Recipes(context.Background(), "i")
	if err != nil || len(rows) != 2 {
		t.Fatal(rows, err)
	}
	for _, r := range rows {
		want, _ := canonicalN1(docs[r.ID])
		if string(want) != string(r.Document) {
			t.Fatal("null/false/zero or recipe lost", r)
		}
	}
}
