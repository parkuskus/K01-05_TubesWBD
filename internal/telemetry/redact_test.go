package telemetry

import (
	"strings"
	"testing"
)

func TestRedactSQL(t *testing.T) {
	for _, tc := range []struct{ name, query, want string }{
		{"parameters", "SELECT $1, $2 FROM items WHERE id=123", "SELECT $1, $2 FROM items WHERE id=?"},
		{"strings", "SELECT 'private-literal', 'private-''quote', E'private-escape'", "SELECT ?, ?, E?"},
		{"dollar quotes", "DO $body$ BEGIN RAISE NOTICE 'private-body'; END $body$; SELECT $$private-value$$", "DO ?; SELECT ?"},
		{"comments", "SELECT /* private-comment /* private-nested */ */ 1 -- private-tail\n", "SELECT ?"},
		{"identifiers", `SELECT "private-identifier" FROM "private-table"`, "SELECT ? FROM ?"},
		{"unterminated string", "SELECT 'private-unclosed", "SELECT ?"},
		{"unterminated dollar", "SELECT $body$private-unclosed", "SELECT ?"},
		{"ambiguous escaping", `SELECT 'private-backslash\', 'private-next'`, redacted},
		{"escape string", `SELECT E'private-backslash\'private-next'`, redacted},
		{"unicode", "SELECT 私密 FROM items", redacted},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactSQL(tc.query)
			if got != tc.want || strings.Contains(got, "private-") {
				t.Fatalf("redacted query = %q, want %q", got, tc.want)
			}
			if again := RedactSQL(got); again != got {
				t.Fatalf("redaction is not stable: %q became %q", got, again)
			}
		})
	}
}
