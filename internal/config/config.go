package config

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

type Config struct {
	Port                  string
	Environment           string
	PlatformDatabaseURL   string
	CSRFKey               []byte
	DatabaseCredentialKey []byte
	InitialAdminUsername  string
	InitialAdminPassword  string
	SecureCookies         bool
}

func Load() (Config, error) {
	if err := loadEnvFile(".env"); err != nil {
		return Config{}, err
	}
	return parseConfig(os.LookupEnv)
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

func parseConfig(lookup func(string) (string, bool)) (Config, error) {
	getRaw := func(key string) string {
		value, _ := lookup(key)
		return value
	}
	get := func(key string) string {
		return strings.TrimSpace(getRaw(key))
	}

	config := Config{
		Port:                 get("PORT"),
		Environment:          strings.ToLower(get("APP_ENV")),
		PlatformDatabaseURL:  get("PLATFORM_DATABASE_URL"),
		InitialAdminUsername: get("INITIAL_ADMIN_USERNAME"),
		InitialAdminPassword: getRaw("INITIAL_ADMIN_PASSWORD"),
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
	return config, nil
}
