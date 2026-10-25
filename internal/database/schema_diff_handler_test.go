package database

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"if3110.local/db-platform/internal/contracts"
	"if3110.local/db-platform/internal/migration"
)

type changingDiffViewer struct {
	stubViewer
	calls int
}

func (v *changingDiffViewer) Diff(_ context.Context, _ contracts.UserID, _ contracts.DatabaseID, _, _ string) (SchemaDiff, error) {
	v.calls++
	return v.diff, v.err
}

type capturedDraft struct {
	calls      int
	user       contracts.UserID
	id         contracts.DatabaseID
	title, sql string
	err        error
}

func (d *capturedDraft) CreateDraft(_ context.Context, actor contracts.UserID, id contracts.DatabaseID, title, sql string) (migration.MigrationRecord, error) {
	d.calls++
	d.user, d.id, d.title, d.sql = actor, id, title, sql
	return migration.MigrationRecord{Version: 7, Status: migration.StatusDraft}, d.err
}

func TestSchemaDiffDraftRecomputesAndRedirects(t *testing.T) {
	h, handler, _ := testHandlers(t)
	seedViewer(t, h)
	v := &changingDiffViewer{stubViewer: stubViewer{diff: SchemaDiff{SQL: "ALTER TABLE live.items ADD COLUMN name text;", Changes: []SchemaChange{{Kind: "column", Action: "add"}}}}}
	h.viewer = v
	d := &capturedDraft{}
	h.drafts = d
	cookies, token := formToken(t, handler, "/databases/1/schemas/diff?reference=desired&target=live", "editor")
	v.diff.SQL = "ALTER TABLE live.items ADD COLUMN updated text;"
	w := submitForm(handler, "/databases/1/schemas/diff", "editor", url.Values{"reference": {"desired"}, "target": {"live"}, "sql_text": {"DROP DATABASE platform_test"}, "gorilla.csrf.Token": {token}}, cookies)
	if w.Code != 303 || w.Header().Get("Location") != "/databases/1/migrations/7" || d.calls != 1 || d.user != "editor" || d.id != "1" || d.sql != v.diff.SQL || !strings.Contains(d.title, "desired") || !strings.Contains(d.title, "live") || v.calls != 2 {
		t.Fatalf("draft response=%d location=%q calls=%d sql-match=%t diff-calls=%d", w.Code, w.Header().Get("Location"), d.calls, d.sql == v.diff.SQL, v.calls)
	}
}

func TestSchemaDiffDraftDeniesAccessCSRFAndEmptyDiff(t *testing.T) {
	h, handler, _ := testHandlers(t)
	a := seedViewer(t, h)
	v := &changingDiffViewer{stubViewer: stubViewer{diff: SchemaDiff{SQL: "SELECT 1;", Changes: []SchemaChange{{Action: "add"}}}}}
	h.viewer = v
	d := &capturedDraft{}
	h.drafts = d
	cookies, token := formToken(t, handler, "/databases/1/schemas/diff?reference=desired&target=live", "editor")
	initialReads := v.calls
	form := url.Values{"reference": {"desired"}, "target": {"live"}, "gorilla.csrf.Token": {token}}
	for _, tc := range []struct {
		actor string
		want  int
	}{{"viewer", 403}, {"admin", 403}, {"", 401}} {
		if w := submitForm(handler, "/databases/1/schemas/diff", tc.actor, form, cookies); w.Code != tc.want {
			t.Fatalf("%q: got %d want %d", tc.actor, w.Code, tc.want)
		}
	}
	withoutCSRF := url.Values{"reference": {"desired"}, "target": {"live"}}
	if w := submitForm(handler, "/databases/1/schemas/diff", "editor", withoutCSRF, cookies); w.Code != 403 {
		t.Fatalf("no CSRF: %d", w.Code)
	}
	for _, pair := range [][2]string{{"desired", "desired"}, {"pg_catalog", "live"}, {"desired", "missing"}} {
		form.Set("reference", pair[0])
		form.Set("target", pair[1])
		want := 400
		if pair[1] == "missing" {
			want = 404
		}
		if w := submitForm(handler, "/databases/1/schemas/diff", "editor", form, cookies); w.Code != want {
			t.Fatalf("%v: got %d want %d", pair, w.Code, want)
		}
	}
	if d.calls != 0 {
		t.Fatal("denied/invalid request created draft")
	}
	if v.calls != initialReads {
		t.Fatalf("denied/invalid request read diff %d times", v.calls-initialReads)
	}
	form.Set("reference", "desired")
	form.Set("target", "live")
	v.diff = SchemaDiff{Unsupported: []SchemaObject{{Name: "view", Kind: "view"}}}
	if w := submitForm(handler, "/databases/1/schemas/diff", "editor", form, cookies); w.Code != 400 || d.calls != 0 {
		t.Fatalf("unsupported-only created draft: %d / %d", w.Code, d.calls)
	}
	v.diff = SchemaDiff{SQL: "SELECT 1;"}
	if w := submitForm(handler, "/databases/1/schemas/diff", "editor", form, cookies); w.Code != 400 || d.calls != 0 {
		t.Fatalf("no changes created draft: %d / %d", w.Code, d.calls)
	}
	v.diff = SchemaDiff{SQL: "SELECT 1;", Changes: []SchemaChange{{Action: "add"}}}
	d.err = errors.New("sensitive storage error")
	if w := submitForm(handler, "/databases/1/schemas/diff", "editor", form, cookies); w.Code != 503 || d.calls != 1 || strings.Contains(w.Body.String(), "sensitive") {
		t.Fatalf("storage error leaked or bypassed: %d / %d", w.Code, d.calls)
	}
	d.err = nil
	v.err = errors.New("sensitive SQL error")
	if w := submitForm(handler, "/databases/1/schemas/diff", "editor", form, cookies); w.Code != 503 || strings.Contains(w.Body.String(), "sensitive") {
		t.Fatalf("error leaked: %d", w.Code)
	}
	v.err = nil
	a.grants["editor"]["1"] = contracts.AccessViewOnly
	if w := submitForm(handler, "/databases/1/schemas/diff", "editor", form, cookies); w.Code != 403 {
		t.Fatalf("downgraded actor: %d", w.Code)
	}
}
