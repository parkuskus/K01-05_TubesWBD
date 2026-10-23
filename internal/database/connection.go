// Package database manages registered PostgreSQL connections and their pools.
package database

import (
	"errors"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"if3110.local/db-platform/internal/contracts"
)

var (
	ErrInvalidInput  = errors.New("invalid database connection input")
	ErrInvalidConfig = errors.New("database manager requires storage, authorizer, and a 32-byte encryption key")
	ErrNotFound      = errors.New("database connection not found")
	ErrConflict      = errors.New("database connection name already exists")
	ErrInUse         = errors.New("database connection is still referenced")
	ErrUnavailable   = errors.New("database connection service unavailable")
	ErrCredentials   = errors.New("database credentials could not be decrypted")
	ErrConnection    = errors.New("could not connect to target database")
	ErrClosed        = errors.New("database manager is closed")
)

// Connection contains displayable metadata only, never a password or DSN.
type Connection struct {
	ID        contracts.DatabaseID
	Name      string
	Host      string
	Port      int
	Database  string
	Username  string
	SSLMode   string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ConnectionInput replaces all metadata. Password is required on create;
// nil on update preserves the stored password. An explicit empty password is rejected.
type ConnectionInput struct {
	Name     string
	Host     string
	Port     int
	Database string
	Username string
	SSLMode  string
	Password *string `json:"-"`
}

func (ConnectionInput) String() string      { return "database.ConnectionInput{credentials redacted}" }
func (in ConnectionInput) GoString() string { return in.String() }

func (in ConnectionInput) validate(create bool) (ConnectionInput, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.Host = strings.TrimSpace(in.Host)
	if !validText(in.Name, 100) || !validText(in.Database, 63) || !validText(in.Username, 63) ||
		!validHost(in.Host) || in.Port < 1 || in.Port > 65535 {
		return ConnectionInput{}, ErrInvalidInput
	}
	switch in.SSLMode {
	case "disable", "require", "verify-full":
	default:
		return ConnectionInput{}, ErrInvalidInput
	}
	if create && in.Password == nil {
		return ConnectionInput{}, ErrInvalidInput
	}
	if in.Password != nil && (len(*in.Password) == 0 || len(*in.Password) > 4096 || strings.ContainsRune(*in.Password, 0) || !utf8.ValidString(*in.Password)) {
		return ConnectionInput{}, ErrInvalidInput
	}
	return in, nil
}

func validText(s string, limit int) bool {
	return strings.TrimSpace(s) != "" && len(s) <= limit && utf8.ValidString(s) &&
		strings.IndexFunc(s, unicode.IsControl) == -1
}

func validHost(host string) bool {
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	if net.ParseIP(host) != nil {
		return true
	}
	// Only a single TCP host, not a DSN, socket path, or multi-host list.
	for _, label := range strings.Split(strings.TrimSuffix(host, "."), ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

func validID(id contracts.DatabaseID) bool {
	n, err := strconv.ParseInt(string(id), 10, 64)
	return err == nil && n > 0 && strconv.FormatInt(n, 10) == string(id)
}
