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
