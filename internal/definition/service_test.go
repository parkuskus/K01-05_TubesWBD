package definition_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"if3110.local/db-platform/internal/contracts"
	"if3110.local/db-platform/internal/definition"
)

type grants struct{ allow bool }

func (g grants) RequirePlatformAdmin(context.Context, contracts.UserID) error {
	return contracts.ErrForbidden
}
func (g grants) RequireDatabaseAccess(_ context.Context, _ contracts.UserID, _ contracts.DatabaseID, _ contracts.DatabaseAction) error {
	if !g.allow {
		return contracts.ErrForbidden
	}
	return nil
}

func TestDefinitionInputValidation(t *testing.T) {
	for _, input := range []definition.Input{
		{Title: "", UpSQL: "SELECT 1", DownSQL: "SELECT 2", Mode: "NORMAL"},
		{Title: "A", UpSQL: "", DownSQL: "SELECT 2", Mode: "NORMAL"},
		{Title: "A", UpSQL: "SELECT 1", Mode: "NORMAL"},
		{Title: "A", UpSQL: "SELECT 1", DownSQL: "SELECT 2", Irreversible: true, Reason: "because", Mode: "NORMAL"},
		{Title: "A", UpSQL: "SELECT 1", Irreversible: true, Reason: "", Mode: "NORMAL"},
		{Title: "A", UpSQL: "SELECT 1", DownSQL: "SELECT 2", Mode: "BATCH"},
		{Title: "A", UpSQL: "SELECT 1", DownSQL: "SELECT 2", Mode: "NORMAL", BatchPlan: []byte(`{"batch_size":100}`)},
		{Title: "A\x00B", UpSQL: "SELECT 1", DownSQL: "SELECT 2", Mode: "NORMAL"},
		{Title: "A", UpSQL: "SELECT \x001", DownSQL: "SELECT 2", Mode: "NORMAL"},
	} {
		if err := input.Validate(); !errors.Is(err, definition.ErrInvalidInput) {
			t.Fatalf("invalid input accepted: %+v: %v", input, err)
		}
	}
	for _, input := range []definition.Input{
		{Title: "A", UpSQL: "SELECT 1", DownSQL: "SELECT 2", Mode: "NORMAL"},
		{Title: "A", UpSQL: "SELECT 1", Irreversible: true, Reason: "Not reversible", Mode: "BATCH", BatchPlan: []byte(`{"batch_size":100}`)},
	} {
		if err := input.Validate(); err != nil {
			t.Fatalf("valid input refused: %v", err)
		}
	}
}

func TestDefinitionValidationReport(t *testing.T) {
	tests := []struct {
		name string
		in   definition.Input
		code string
	}{
		{
			name: "normal SQL passes with compatibility warning",
			in: definition.Input{
				Title: "Add index", UpSQL: "CREATE INDEX users_email_idx ON users(email)",
				DownSQL: "DROP INDEX users_email_idx", Mode: "NORMAL", Notes: "Coordinate with application release.",
			},
		},
		{
			name: "batch update passes",
			in: definition.Input{
				Title: "Batch", UpSQL: "UPDATE public.users SET active = false WHERE active = true",
				DownSQL: "UPDATE public.users SET active = true WHERE active = false", Mode: "BATCH",
				BatchPlan: []byte(`{"table_schema":"public","table_name":"users","primary_key_column":"id","scope_where":"active = true","batch_size":100}`),
			},
		},
		{
			name: "transaction control is rejected",
			in:   definition.Input{Title: "Unsafe", UpSQL: "BEGIN; SELECT 1", DownSQL: "SELECT 2", Mode: "NORMAL"},
			code: "UNSAFE_SQL",
		},
		{
			name: "internal schema access is rejected",
			in:   definition.Input{Title: "Unsafe", UpSQL: "SELECT * FROM if3110_platform.jobs", DownSQL: "SELECT 2", Mode: "NORMAL"},
			code: "UNSAFE_SQL",
		},
		{
			name: "batch plan requires known config fields",
			in: definition.Input{
				Title: "Batch", UpSQL: "UPDATE users SET active = false", DownSQL: "UPDATE users SET active = true", Mode: "BATCH",
				BatchPlan: []byte(`{"schema":"public","batch_size":100}`),
			},
			code: "INVALID_BATCH_CONFIG",
		},
		{
			name: "batch plan rejects duplicate keys",
			in: definition.Input{
				Title: "Batch", UpSQL: "UPDATE users SET active = false", DownSQL: "UPDATE users SET active = true", Mode: "BATCH",
				BatchPlan: []byte(`{"table_schema":"public","table_name":"users","table_name":"other","primary_key_column":"id","batch_size":100}`),
			},
			code: "INVALID_BATCH_CONFIG",
		},
		{
			name: "batch size above the initial safe cap is rejected",
			in: definition.Input{
				Title: "Batch", UpSQL: "UPDATE users SET active = false", DownSQL: "UPDATE users SET active = true", Mode: "BATCH",
				BatchPlan: []byte(`{"table_schema":"public","table_name":"users","primary_key_column":"id","batch_size":10001}`),
			},
			code: "INVALID_BATCH_CONFIG",
		},
		{
			name: "batch scope rejects statements",
			in: definition.Input{
				Title: "Batch", UpSQL: "UPDATE users SET active = false", DownSQL: "UPDATE users SET active = true", Mode: "BATCH",
				BatchPlan: []byte(`{"table_schema":"public","table_name":"users","primary_key_column":"id","scope_where":"active = true; DROP TABLE users","batch_size":100}`),
			},
			code: "INVALID_BATCH_SCOPE",
		},
		{
			name: "batch up SQL must be a supported update",
			in: definition.Input{
				Title: "Batch", UpSQL: "DELETE FROM users", DownSQL: "UPDATE users SET active = true", Mode: "BATCH",
				BatchPlan: []byte(`{"table_schema":"public","table_name":"users","primary_key_column":"id","batch_size":100}`),
			},
			code: "INVALID_BATCH_SQL",
		},
		{
			name: "batch update table must match frozen scope",
			in: definition.Input{
				Title: "Batch", UpSQL: "UPDATE public.payments SET active = false", DownSQL: "UPDATE public.payments SET active = true", Mode: "BATCH",
				BatchPlan: []byte(`{"table_schema":"public","table_name":"users","primary_key_column":"id","batch_size":100}`),
			},
			code: "INVALID_BATCH_SQL",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			report := definition.ValidateInput(tc.in)
			if report.Valid != (tc.code == "") {
				t.Fatalf("Valid = %v, errors=%+v", report.Valid, report.Errors)
			}
			if tc.code != "" && (len(report.Errors) == 0 || report.Errors[0].Code != tc.code) {
				t.Fatalf("first issue = %+v, want code %q", report.Errors, tc.code)
			}
			if tc.code == "" {
				if report.CompatibilityNotes != tc.in.Notes || len(report.Warnings) == 0 || report.Warnings[0].Code != "COMPATIBILITY_UNVERIFIED" {
					t.Fatalf("report must preserve notes and require manual compatibility review: %+v", report)
				}
			}
		})
	}
}

func TestDefinitionAccessBeforeStorage(t *testing.T) {
	s, err := definition.NewService(&sql.DB{}, grants{})
	if err != nil {
		t.Fatal(err)
	}
	input := definition.Input{Title: "A", UpSQL: "SELECT 1", DownSQL: "SELECT 2", Mode: "NORMAL"}
	if _, err := s.Create(context.Background(), "2", "1", input); !errors.Is(err, contracts.ErrForbidden) {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.List(context.Background(), "2", "1"); !errors.Is(err, contracts.ErrForbidden) {
		t.Fatalf("list: %v", err)
	}
	if _, err := s.Get(context.Background(), "2", "1", "1"); !errors.Is(err, contracts.ErrForbidden) {
		t.Fatalf("get: %v", err)
	}
	if _, err := s.Revise(context.Background(), "2", "1", "1", 1, input); !errors.Is(err, contracts.ErrForbidden) {
		t.Fatalf("revise: %v", err)
	}
}
