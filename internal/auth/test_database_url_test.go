package auth

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/joho/godotenv"
)

func testDatabaseURL(dotenvPath string, lookup func(string) (string, bool)) (string, error) {
	if dsn, ok := lookup("PLATFORM_TEST_DATABASE_URL"); ok && dsn != "" {
		return dsn, nil
	}
	values, err := godotenv.Read(dotenvPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", errors.New("could not load test environment file")
	}
	return values["PLATFORM_TEST_DATABASE_URL"], nil
}

func TestTestDatabaseURLFallsBackToDotEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("PLATFORM_TEST_DATABASE_URL=postgres://test:test@localhost/platform_test?sslmode=disable\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := testDatabaseURL(path, func(string) (string, bool) { return "", false })
	if err != nil {
		t.Fatal(err)
	}
	if want := "postgres://test:test@localhost/platform_test?sslmode=disable"; got != want {
		t.Fatalf("testDatabaseURL() = %q, want dotenv value", got)
	}
}

func TestTestDatabaseURLPrefersProcessEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("PLATFORM_TEST_DATABASE_URL=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := testDatabaseURL(path, func(key string) (string, bool) {
		if key == "PLATFORM_TEST_DATABASE_URL" {
			return "from-process", true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != "from-process" {
		t.Fatalf("testDatabaseURL() = %q, want process value", got)
	}
}
