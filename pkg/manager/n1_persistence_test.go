package manager

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"llamactl/pkg/database"
	"llamactl/pkg/instance"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
)

func TestN1LookupErrorsAreNotAbsence(t *testing.T) {
	im, inst, _, _ := p3Manager(t)
	db, err := database.Open(&database.Config{Path: ":memory:", MaxOpenConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = database.RunMigrations(db); err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = conn.Raw(func(raw any) error {
		raw.(*sqlite3.SQLiteConn).RegisterAuthorizer(func(op int, a, b, c string) int {
			if op == sqlite3.SQLITE_READ && (a == "recipes" || a == "execution_runs") {
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
	n := &n1Coordinator{store: db.N1Store()}
	s := inst.ControlSnapshot()
	req := n1JSON(map[string]any{"boot_id": s.BootID, "expected_revision": s.Revision, "idempotency_key": "lookup", "artifact_id": "missing"})
	_, _, err = im.n1Inventory(context.Background(), n, inst, "scope", "POST", []string{"inventory", "register"}, nil, req)
	if err == nil || errors.Is(err, sql.ErrNoRows) {
		t.Errorf("register storage error became absence: %v", err)
	}
	doc := N1Recipe{Tags: []string{}, Description: "synthetic import", ArtifactDigest: "synthetic", Engine: "synthetic", Deployment: map[string]json.RawMessage{}, Request: instance.RequestContract{TemplateVersion: "v1", Parameters: map[string]json.RawMessage{"max_tokens": json.RawMessage(`8`)}}, Conclusion: "unverified"}
	req = n1JSON(map[string]any{"dry_run": true, "snapshot": map[string]any{"schema_version": 1, "recipe_id": "missing", "version": 1, "recipe_digest": database.N1Digest(n1JSON(doc)), "recipe": doc}})
	_, _, err = im.n1Import(context.Background(), n, inst, "scope", req)
	if err == nil || errors.Is(err, sql.ErrNoRows) {
		t.Errorf("import storage error became absence: %v", err)
	}
}

func TestN1ReadyPersistencePrecedesLiveAdmission(t *testing.T) {
	im, inst, _, _ := p3Manager(t)
	ready := p3Submit(t, im, inst, "ready-first", "apply", "alpha", `{}`)
	if ready.State != "succeeded" {
		t.Fatal(ready)
	}
	db, err := database.Open(&database.Config{Path: ":memory:", MaxOpenConnections: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = database.RunMigrations(db); err != nil {
		t.Fatal(err)
	}
	s := inst.ControlSnapshot()
	reservation, err := inst.ReserveN1("use", "scope", "service_use", s.BootID, s.Revision, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	defer reservation.CloseConfirmed()
	r, _, err := db.N1Store().Accept(context.Background(), database.ExecutionRun{ID: "use", Kind: "switch", Scope: "scope", Instance: inst.Name, Boot: s.BootID, Key: "use", Payload: json.RawMessage(`{}`), Manifest: json.RawMessage(`{}`), Deadline: time.Now().Add(time.Minute).UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	contract := instance.RequestContract{TemplateVersion: "v1", Parameters: map[string]json.RawMessage{"max_tokens": json.RawMessage(`8`)}}
	n := &n1Coordinator{store: db.N1Store()}
	l := &n1Live{run: r, inst: inst, reservation: reservation, borrowed: true, busy: true, recipe: N1Recipe{ArtifactID: "alpha", Request: contract}, record: database.RecipeRecord{ID: "recipe", Version: 1}, use: n1Use{IdleSeconds: 60}}
	n.active = l
	observed := 0
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = conn.Raw(func(raw any) error {
		return raw.(*sqlite3.SQLiteConn).RegisterFunc("probe_ready", func() (int, error) {
			req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"alpha","messages":[{"role":"user","content":"synthetic"}]}`))
			req.Header.Set("X-Service-Use", "use")
			req.Header.Set("X-Ready-Token", inst.ControlSnapshot().ReadyToken)
			req.Header.Set("X-Prompt-Template-Version", "v1")
			req.Header.Set("X-Prompt-Digest", contract.PromptDigest())
			req = instance.WithServiceScope(req, "scope")
			w := httptest.NewRecorder()
			e := inst.ServeHTTP(w, req)
			observed = w.Code
			if e != nil {
				observed = 409
			}
			return 0, errors.New("injected ready persistence failure")
		}, false)
	})
	conn.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TRIGGER fail_ready BEFORE INSERT ON execution_events WHEN NEW.event_type='ready' BEGIN SELECT probe_ready(); END`); err != nil {
		t.Fatal(err)
	}
	n.wg.Add(1)
	im.n1Switch(n, l)
	n.wg.Wait()
	if observed == 0 {
		t.Fatal("probe did not run")
	}
	if observed < 400 {
		t.Fatalf("live inference admitted during failed ready persistence: HTTP %d", observed)
	}
	events, err := n.store.Events(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Type == "ready" {
			t.Fatal("failed ready persisted")
		}
	}
}
