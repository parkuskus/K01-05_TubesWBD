package config

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	Port                   string
	Environment            string
	PlatformDatabaseURL    string
	CSRFKey                []byte
	DatabaseCredentialKey  []byte
	InitialAdminUsername   string
	InitialAdminPassword   string
	SecureCookies          bool
	FrontendOrigins        []string
	WorkerID               string
	WorkerConcurrency      int
	WorkerPollInterval     time.Duration
	JobLeaseDuration       time.Duration
	JobHeartbeatInterval   time.Duration
	JobMaxRetries          int
	TargetExecutionTimeout time.Duration
}

func Load() (Config, error) {
	if err := loadEnvFile(".env"); err != nil {
		return Config{}, err
	}
	return parseConfig(os.LookupEnv)
}

// LoadPlatformDatabaseURL loads .env if present and returns PLATFORM_DATABASE_URL
// for standalone schema migration commands such as cmd/migrate.
func LoadPlatformDatabaseURL() (string, error) {
	if err := loadEnvFile(".env"); err != nil {
		return "", err
	}
	return parsePlatformDatabaseURL(os.LookupEnv)
}

func parsePlatformDatabaseURL(lookup func(string) (string, bool)) (string, error) {
	raw, _ := lookup("PLATFORM_DATABASE_URL")
	dsn := strings.TrimSpace(raw)
	if dsn == "" {
		return "", errors.New("PLATFORM_DATABASE_URL is required")
	}
	return dsn, nil
}

func loadEnvFile(path string) error {
	err := godotenv.Load(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("could not load environment file")
	}
	return nil
}

func defaultWorkerID() string {
	host, err := os.Hostname()
	host = strings.TrimSpace(host)
	if err != nil || host == "" {
		host = "worker"
	}
	return fmt.Sprintf("%s-%d", host, os.Getpid())
}

func parseConfig(lookup func(string) (string, bool)) (Config, error) {
	getRaw := func(key string) string {
		value, _ := lookup(key)
		return value
	}
	get := func(key string) string {
		return strings.TrimSpace(getRaw(key))
	}

	config := Config{
		Port:                   get("PORT"),
		Environment:            strings.ToLower(get("APP_ENV")),
		PlatformDatabaseURL:    get("PLATFORM_DATABASE_URL"),
		InitialAdminUsername:   get("INITIAL_ADMIN_USERNAME"),
		InitialAdminPassword:   getRaw("INITIAL_ADMIN_PASSWORD"),
		WorkerID:               get("WORKER_ID"),
		WorkerConcurrency:      2,
		WorkerPollInterval:     time.Second,
		JobLeaseDuration:       30 * time.Second,
		JobHeartbeatInterval:   10 * time.Second,
		JobMaxRetries:          3,
		TargetExecutionTimeout: 10 * time.Minute,
	}
	if config.Port == "" {
		config.Port = "8080"
	}
	port, err := strconv.Atoi(config.Port)
	if err != nil || port < 1 || port > 65535 {
		return Config{}, errors.New("PORT must be a valid TCP port")
	}
	if config.Environment == "" {
		config.Environment = "development"
	}
	if config.Environment != "development" && config.Environment != "test" && config.Environment != "production" {
		return Config{}, errors.New("APP_ENV must be development, test, or production")
	}
	if config.PlatformDatabaseURL == "" {
		return Config{}, errors.New("PLATFORM_DATABASE_URL is required")
	}
	if config.CSRFKey, err = hex.DecodeString(get("CSRF_AUTH_KEY")); err != nil || len(config.CSRFKey) != 32 {
		return Config{}, errors.New("CSRF_AUTH_KEY must be 64 hexadecimal characters")
	}
	if config.DatabaseCredentialKey, err = hex.DecodeString(get("DATABASE_CREDENTIAL_KEY")); err != nil || len(config.DatabaseCredentialKey) != 32 {
		return Config{}, errors.New("DATABASE_CREDENTIAL_KEY must be 64 hexadecimal characters")
	}
	if bytes.Equal(config.DatabaseCredentialKey, config.CSRFKey) {
		return Config{}, errors.New("DATABASE_CREDENTIAL_KEY must be distinct from CSRF_AUTH_KEY")
	}
	if secure := get("SECURE_COOKIES"); secure != "" {
		config.SecureCookies, err = strconv.ParseBool(secure)
		if err != nil {
			return Config{}, errors.New("SECURE_COOKIES must be true or false")
		}
	}
	if config.Environment == "production" && !config.SecureCookies {
		return Config{}, errors.New("SECURE_COOKIES must be true in production")
	}
	if origin := get("FRONTEND_ORIGIN"); origin != "" {
		parsed, err := url.Parse(origin)
		if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" ||
			(parsed.Scheme != "http" && parsed.Scheme != "https") || (config.Environment == "production" && parsed.Scheme != "https") {
			return Config{}, errors.New("FRONTEND_ORIGIN must be an HTTP origin without credentials or a path; production requires HTTPS")
		}
		config.FrontendOrigins = []string{parsed.Scheme + "://" + parsed.Host}
	} else if config.Environment == "development" {
		config.FrontendOrigins = []string{"http://localhost:3000", "http://127.0.0.1:3000"}
	}

	if config.WorkerID == "" {
		config.WorkerID = defaultWorkerID()
	}
	if rawConcurrency := get("WORKER_CONCURRENCY"); rawConcurrency != "" {
		concurrency, err := strconv.Atoi(rawConcurrency)
		if err != nil || concurrency < 1 {
			return Config{}, errors.New("WORKER_CONCURRENCY must be a positive integer")
		}
		config.WorkerConcurrency = concurrency
	}
	if rawPoll := get("WORKER_POLL_INTERVAL"); rawPoll != "" {
		poll, err := time.ParseDuration(rawPoll)
		if err != nil || poll <= 0 {
			return Config{}, errors.New("WORKER_POLL_INTERVAL must be a positive duration")
		}
		config.WorkerPollInterval = poll
	}
	if rawLease := get("JOB_LEASE_DURATION"); rawLease != "" {
		lease, err := time.ParseDuration(rawLease)
		if err != nil || lease <= 0 {
			return Config{}, errors.New("JOB_LEASE_DURATION must be a positive duration")
		}
		config.JobLeaseDuration = lease
	}
	if rawHeartbeat := get("JOB_HEARTBEAT_INTERVAL"); rawHeartbeat != "" {
		heartbeat, err := time.ParseDuration(rawHeartbeat)
		if err != nil || heartbeat <= 0 {
			return Config{}, errors.New("JOB_HEARTBEAT_INTERVAL must be a positive duration")
		}
		config.JobHeartbeatInterval = heartbeat
	}
	if config.JobHeartbeatInterval >= config.JobLeaseDuration {
		return Config{}, errors.New("JOB_HEARTBEAT_INTERVAL must be less than JOB_LEASE_DURATION")
	}
	if rawMaxRetries := get("JOB_MAX_RETRIES"); rawMaxRetries != "" {
		maxRetries, err := strconv.Atoi(rawMaxRetries)
		if err != nil || maxRetries < 0 {
			return Config{}, errors.New("JOB_MAX_RETRIES must be a non-negative integer")
		}
		config.JobMaxRetries = maxRetries
	}
	if rawExecTimeout := get("TARGET_EXECUTION_TIMEOUT"); rawExecTimeout != "" {
		execTimeout, err := time.ParseDuration(rawExecTimeout)
		if err != nil || execTimeout <= 0 {
			return Config{}, errors.New("TARGET_EXECUTION_TIMEOUT must be a positive duration")
		}
		config.TargetExecutionTimeout = execTimeout
	}

	return config, nil
}
