package definition

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"if3110.local/db-platform/internal/contracts"
)

var (
	ErrInvalidInput = errors.New("invalid migration definition")
	ErrNotFound     = errors.New("migration definition not found")
	ErrConflict     = errors.New("migration definition revision conflict")
	ErrUnavailable  = errors.New("migration definition storage unavailable")
)

type Input struct {
	Title        string          `json:"title"`
	UpSQL        string          `json:"up_sql"`
	DownSQL      string          `json:"down_sql"`
	Irreversible bool            `json:"irreversible"`
	Reason       string          `json:"reason"`
	Mode         string          `json:"mode"`
	BatchPlan    json.RawMessage `json:"batch_plan"`
	Notes        string          `json:"notes"`
}

func (in Input) Validate() error {
	if len(strings.TrimSpace(in.Title)) == 0 || len(in.Title) > 200 || !utf8.ValidString(in.Title) ||
		len(strings.TrimSpace(in.UpSQL)) == 0 || len(in.UpSQL) > 1048576 || !utf8.ValidString(in.UpSQL) ||
		len(in.DownSQL) > 1048576 || !utf8.ValidString(in.DownSQL) || len(in.Reason) > 2000 || !utf8.ValidString(in.Reason) ||
		len(in.Notes) > 8192 || !utf8.ValidString(in.Notes) ||
		(in.Irreversible && (in.DownSQL != "" || strings.TrimSpace(in.Reason) == "")) ||
		(!in.Irreversible && (strings.TrimSpace(in.DownSQL) == "" || in.Reason != "")) {
		return ErrInvalidInput
	}
	if in.Mode != "NORMAL" && in.Mode != "BATCH" {
		return ErrInvalidInput
	}
	plan := bytes.TrimSpace(in.BatchPlan)
	if in.Mode == "NORMAL" {
		if len(plan) > 0 && !bytes.Equal(plan, []byte(`{}`)) {
			return ErrInvalidInput
		}
		return nil
	}
	if len(plan) == 0 || len(plan) > 8192 || !json.Valid(plan) || plan[0] != '{' {
		return ErrInvalidInput
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(plan, &object) != nil || len(object) == 0 {
		return ErrInvalidInput
	}
	return nil
}

type Definition struct {
	ID                 string               `json:"id"`
	DatabaseID         contracts.DatabaseID `json:"database_id"`
	ConnectionRevision int64                `json:"connection_revision"`
	Version            int64                `json:"version"`
	State              string               `json:"state"`
	Revision           int                  `json:"revision"`
	CreatedBy          contracts.UserID     `json:"created_by"`
	AttemptedAt        *time.Time           `json:"attempted_at"`
	CreatedAt          time.Time            `json:"created_at"`
	UpdatedAt          time.Time            `json:"updated_at"`
	Input
	Checksum string `json:"checksum"`
}

func validID(id string) bool {
	n, err := strconv.ParseInt(id, 10, 64)
	return err == nil && n > 0 && strconv.FormatInt(n, 10) == id
}

func checksum(dbID contracts.DatabaseID, connectionRevision int64, in Input) string {
	if len(in.BatchPlan) == 0 {
		in.BatchPlan = []byte(`{}`)
	}
	// Encode values with boundaries rather than concatenating SQL fields.
	data, _ := json.Marshal([]any{dbID, connectionRevision, in.Title, in.UpSQL, in.DownSQL, in.Irreversible, in.Reason, in.Mode, json.RawMessage(in.BatchPlan), in.Notes})
	return fmt.Sprintf("%x", sha256.Sum256(data))
}
