package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseConfigRequiresPlatformDatabaseAndCSRFKey(t *testing.T) {
	validKey := strings.Repeat("ab", 32)
	for _, env := range []map[string]string{
		{"CSRF_AUTH_KEY": validKey},
		{"PLATFORM_DATABASE_URL": "postgres://localhost/platform_dev"},
	} {
		if _, err := parseConfig(func(key string) (string, bool) {
			value, ok := env[key]
			return value, ok
		}); err == nil {
			t.Fatalf("parseConfig(%v) succeeded without all required settings", env)
		}
	}
}

func TestParseConfigValidatesCSRFKeyAndCookieFlag(t *testing.T) {
	validKey := strings.Repeat("ab", 32)
	base := map[string]string{
		"PLATFORM_DATABASE_URL": "postgres://localhost/platform_dev",
		"CSRF_AUTH_KEY":         validKey,
		"SECURE_COOKIES":        "true",
	}
	config, err := parseConfig(func(key string) (string, bool) {
		value, ok := base[key]
		return value, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(config.CSRFKey) != 32 || config.Port != "8080" || config.Environment != "development" || !config.SecureCookies {
		t.Fatalf("parsed config has unexpected defaults/settings: %+v", config)
	}

	base["CSRF_AUTH_KEY"] = "not-hex"
	if _, err := parseConfig(func(key string) (string, bool) {
		value, ok := base[key]
		return value, ok
	}); err == nil {
		t.Fatal("invalid CSRF key encoding was accepted")
	}
	base["CSRF_AUTH_KEY"] = validKey
	base["SECURE_COOKIES"] = "sometimes"
	if _, err := parseConfig(func(key string) (string, bool) {
		value, ok := base[key]
		return value, ok
	}); err == nil {
		t.Fatal("invalid SECURE_COOKIES value was accepted")
	}
	base["SECURE_COOKIES"] = "false"
	base["APP_ENV"] = "production"
	if _, err := parseConfig(func(key string) (string, bool) {
		value, ok := base[key]
		return value, ok
	}); err == nil {
		t.Fatal("production config accepted insecure cookies")
	}
}

func TestParseConfigPreservesBootstrapPasswordWhitespace(t *testing.T) {
	env := map[string]string{
		"PLATFORM_DATABASE_URL":  "postgres://localhost/platform_dev",
		"CSRF_AUTH_KEY":          strings.Repeat("ab", 32),
		"INITIAL_ADMIN_USERNAME": "admin",
		"INITIAL_ADMIN_PASSWORD": " leading and trailing ",
	}
	config, err := parseConfig(func(key string) (string, bool) {
		value, ok := env[key]
		return value, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	if config.InitialAdminPassword != env["INITIAL_ADMIN_PASSWORD"] {
		t.Fatalf("bootstrap password was modified: got %q", config.InitialAdminPassword)
	}
}

func TestLoadEnvFileDoesNotOverrideProcessEnvironment(t *testing.T) {
	processKey := "PLATFORM_CONFIG_TEST_PROCESS_VALUE"
	fileKey := "PLATFORM_CONFIG_TEST_FILE_VALUE"
	t.Setenv(processKey, "from-process")
	original, wasSet := os.LookupEnv(fileKey)
	if err := os.Unsetenv(fileKey); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if wasSet {
			_ = os.Setenv(fileKey, original)
		} else {
			_ = os.Unsetenv(fileKey)
		}
	})
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(processKey+"=from-file\n"+fileKey+"=loaded-from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv(processKey); got != "from-process" {
		t.Fatalf("environment value = %q, want process value", got)
	}
	if got := os.Getenv(fileKey); got != "loaded-from-file" {
		t.Fatalf("file environment value = %q, want loaded-from-file", got)
	}
}
