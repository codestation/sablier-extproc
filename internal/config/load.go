// Package config loads and validates the service configuration.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// EnvPrefix prefixes environment variables used to override configuration.
const EnvPrefix = "SABLIER_EXTPROC_"

const maxConfigFileBytes = 4 * 1024 * 1024

// LookupEnv retrieves an environment variable by name.
type LookupEnv func(string) (string, bool)

// ResolvePath returns the configured file path or the supplied default.
func ResolvePath(defaultPath string, lookup LookupEnv) string {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	if value, ok := lookup(EnvPrefix + "CONFIG_FILE"); ok && strings.TrimSpace(value) != "" {
		return value
	}
	return defaultPath
}

// Load reads and validates a configuration file using the process environment.
func Load(path string) (Config, error) {
	return LoadWithEnv(path, os.LookupEnv)
}

// LoadWithEnv reads and validates a configuration file using the supplied environment lookup.
func LoadWithEnv(path string, lookup LookupEnv) (Config, error) {
	contents, err := readConfigFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}

	cfg := defaults()
	decoder := yaml.NewDecoder(bytes.NewReader(contents))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Config{}, errors.New("decode config: multiple YAML documents are not allowed")
		}
		return Config{}, fmt.Errorf("decode config: %w", err)
	}

	if lookup == nil {
		lookup = func(string) (string, bool) { return "", false }
	}
	if err := applyEnvironment(&cfg, lookup); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func readConfigFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	contents, readErr := io.ReadAll(io.LimitReader(file, maxConfigFileBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	if len(contents) > maxConfigFileBytes {
		return nil, fmt.Errorf("file exceeds %d bytes", maxConfigFileBytes)
	}
	return contents, nil
}

func applyEnvironment(cfg *Config, lookup LookupEnv) error {
	stringOverrides := []struct {
		name   string
		target *string
	}{
		{"GRPC_LISTEN_ADDRESS", &cfg.Server.GRPCListenAddress},
		{"ADMIN_LISTEN_ADDRESS", &cfg.Server.AdminListenAddress},
		{"SABLIER_BASE_URL", &cfg.Sablier.BaseURL},
		{"LOG_LEVEL", &cfg.Logging.Level},
	}
	for _, override := range stringOverrides {
		if value, ok := lookup(EnvPrefix + override.name); ok {
			*override.target = value
		}
	}

	durationOverrides := []struct {
		name   string
		target *Duration
	}{
		{"SABLIER_REQUEST_TIMEOUT", &cfg.Sablier.RequestTimeout},
		{"SESSION_DURATION", &cfg.Sablier.SessionDuration},
		{"RETRY_AFTER", &cfg.Sablier.RetryAfter},
	}
	for _, override := range durationOverrides {
		value, ok := lookup(EnvPrefix + override.name)
		if !ok {
			continue
		}
		parsed, err := time.ParseDuration(value)
		if err != nil {
			return fmt.Errorf("environment %s%s: %w", EnvPrefix, override.name, err)
		}
		*override.target = Duration(parsed)
	}

	if value, ok := lookup(EnvPrefix + "MAX_RESPONSE_BYTES"); ok {
		parsed, err := parseByteSize(value)
		if err != nil {
			return fmt.Errorf("environment %sMAX_RESPONSE_BYTES: %w", EnvPrefix, err)
		}
		cfg.Sablier.MaxResponseBytes = parsed
	}
	if value, ok := lookup(EnvPrefix + "FAILURE_POLICY"); ok {
		cfg.Sablier.FailurePolicy = FailurePolicy(value)
	}
	if value, ok := lookup(EnvPrefix + "VERSION"); ok {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("environment %sVERSION: %w", EnvPrefix, err)
		}
		cfg.Version = parsed
	}
	return nil
}
