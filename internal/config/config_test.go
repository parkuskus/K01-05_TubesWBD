package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseConfigRequiresPlatformDatabaseCSRFAndCredentialKeys(t *testing.T) {
	validCSRFKey := strings.Repeat("ab", 32)
	validCredKey := strings.Repeat("cd", 32)
	for _, env := range []map[string]string{
		{"CSRF_AUTH_KEY": validCSRFKey, "DATABASE_CREDENTIAL_KEY": validCredKey},
		{"PLATFORM_DATABASE_URL": "postgres://localhost/platform_dev", "DATABASE_CREDENTIAL_KEY": validCredKey},
		{"PLATFORM_DATABASE_URL": "postgres://localhost/platform_dev", "CSRF_AUTH_KEY": validCSRFKey},
	} {
		if _, err := parseConfig(func(key string) (string, bool) {
			value, ok := env[key]
			return value, ok
		}); err == nil {
			t.Fatalf("parseConfig(%v) succeeded without all required settings", env)
		}
	}
}

func TestParseConfigValidatesCSRFKeyCredentialKeyAndCookieFlag(t *testing.T) {
	validCSRFKey := strings.Repeat("ab", 32)
	validCredKey := strings.Repeat("cd", 32)
	base := map[string]string{
		"PLATFORM_DATABASE_URL":   "postgres://localhost/platform_dev",
		"CSRF_AUTH_KEY":           validCSRFKey,
		"DATABASE_CREDENTIAL_KEY": validCredKey,
		"SECURE_COOKIES":          "true",
	}
	config, err := parseConfig(func(key string) (string, bool) {
		value, ok := base[key]
		return value, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(config.CSRFKey) != 32 || len(config.DatabaseCredentialKey) != 32 || config.Port != "8080" || config.Environment != "development" || !config.SecureCookies {
		t.Fatalf("parsed config has unexpected defaults/settings: %+v", config)
	}

	base["CSRF_AUTH_KEY"] = "not-hex"
	if _, err := parseConfig(func(key string) (string, bool) {
		value, ok := base[key]
		return value, ok
	}); err == nil {
		t.Fatal("invalid CSRF key encoding was accepted")
	}
	base["CSRF_AUTH_KEY"] = validCSRFKey

	for _, badCredKey := range []string{"", "not-hex", strings.Repeat("cd", 31), validCSRFKey} {
		base["DATABASE_CREDENTIAL_KEY"] = badCredKey
		if _, err := parseConfig(func(key string) (string, bool) {
			value, ok := base[key]
			return value, ok
		}); err == nil {
			t.Fatalf("invalid DATABASE_CREDENTIAL_KEY %q was accepted", badCredKey)
		}
	}
	base["DATABASE_CREDENTIAL_KEY"] = validCredKey
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
		"PLATFORM_DATABASE_URL":   "postgres://localhost/platform_dev",
		"CSRF_AUTH_KEY":           strings.Repeat("ab", 32),
		"DATABASE_CREDENTIAL_KEY": strings.Repeat("cd", 32),
		"INITIAL_ADMIN_USERNAME":  "admin",
		"INITIAL_ADMIN_PASSWORD":  " leading and trailing ",
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

func TestParseConfigWorkerDefaultsAndOverrides(t *testing.T) {
	base := map[string]string{
		"PLATFORM_DATABASE_URL":   "postgres://localhost/platform_dev",
		"CSRF_AUTH_KEY":           strings.Repeat("ab", 32),
		"DATABASE_CREDENTIAL_KEY": strings.Repeat("cd", 32),
	}
	cfg, err := parseConfig(func(key string) (string, bool) {
		val, ok := base[key]
		return val, ok
	})
	if err != nil {
		t.Fatalf("parseConfig() default worker settings error = %v", err)
	}
	if cfg.WorkerID == "" ||
		cfg.WorkerConcurrency != 2 ||
		cfg.WorkerPollInterval != time.Second ||
		cfg.JobLeaseDuration != 30*time.Second ||
		cfg.JobHeartbeatInterval != 10*time.Second ||
		cfg.JobMaxRetries != 3 ||
		cfg.TargetExecutionTimeout != 10*time.Minute {
		t.Fatalf("unexpected default worker config: %+v", cfg)
	}

	overrides := map[string]string{
		"PLATFORM_DATABASE_URL":    "postgres://localhost/platform_dev",
		"CSRF_AUTH_KEY":            strings.Repeat("ab", 32),
		"DATABASE_CREDENTIAL_KEY":  strings.Repeat("cd", 32),
		"WORKER_ID":                "worker-alpha-1",
		"WORKER_CONCURRENCY":       "4",
		"WORKER_POLL_INTERVAL":     "500ms",
		"JOB_LEASE_DURATION":       "45s",
		"JOB_HEARTBEAT_INTERVAL":   "15s",
		"JOB_MAX_RETRIES":          "5",
		"TARGET_EXECUTION_TIMEOUT": "25m",
	}
	cfg, err = parseConfig(func(key string) (string, bool) {
		val, ok := overrides[key]
		return val, ok
	})
	if err != nil {
		t.Fatalf("parseConfig() custom worker settings error = %v", err)
	}
	if cfg.WorkerID != "worker-alpha-1" ||
		cfg.WorkerConcurrency != 4 ||
		cfg.WorkerPollInterval != 500*time.Millisecond ||
		cfg.JobLeaseDuration != 45*time.Second ||
		cfg.JobHeartbeatInterval != 15*time.Second ||
		cfg.JobMaxRetries != 5 ||
		cfg.TargetExecutionTimeout != 25*time.Minute {
		t.Fatalf("unexpected overridden worker config: %+v", cfg)
	}

	invalidCases := []map[string]string{
		{"WORKER_CONCURRENCY": "0"},
		{"WORKER_CONCURRENCY": "-1"},
		{"WORKER_POLL_INTERVAL": "0s"},
		{"JOB_LEASE_DURATION": "0s"},
		{"JOB_HEARTBEAT_INTERVAL": "30s"}, // equal to default 30s lease
		{"JOB_MAX_RETRIES": "-1"},
		{"TARGET_EXECUTION_TIMEOUT": "0s"},
	}
	for _, tc := range invalidCases {
		env := make(map[string]string, len(base)+len(tc))
		for k, v := range base {
			env[k] = v
		}
		for k, v := range tc {
			env[k] = v
		}
		if _, err := parseConfig(func(key string) (string, bool) {
			val, ok := env[key]
			return val, ok
		}); err == nil {
			t.Fatalf("expected invalid worker setting %v to fail validation", tc)
		}
	}
}
