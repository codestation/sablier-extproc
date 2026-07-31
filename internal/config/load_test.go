package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validConfig = `
version: 1
server:
  grpcListenAddress: ":18080"
  adminListenAddress: ":8080"
sablier:
  baseURL: "http://sablier.sablier-system.svc:10000"
  requestTimeout: 3s
  maxResponseBytes: 1MiB
  sessionDuration: 1h
  failurePolicy: closed
  retryAfter: 5s
logging:
  level: info
mappings:
  - host: "APP.Example.com."
    group: " app "
    exclusions:
      - type: exact
        path: /healthz
    waitingPage:
      refreshFrequency: 2s
`

func TestLoadWithDefaultsAndNormalization(t *testing.T) {
	cfg := loadText(t, validConfig, nil)

	if cfg.Server.GRPCListenAddress != ":18080" {
		t.Fatalf("unexpected gRPC address: %q", cfg.Server.GRPCListenAddress)
	}
	if cfg.Sablier.MaxResponseBytes.Value() != 1024*1024 {
		t.Fatalf("unexpected response limit: %d", cfg.Sablier.MaxResponseBytes.Value())
	}
	if cfg.Mappings[0].Host != "app.example.com" {
		t.Fatalf("host was not normalized: %q", cfg.Mappings[0].Host)
	}
	if cfg.Mappings[0].PathPrefix != "/" {
		t.Fatalf("default path prefix not applied: %q", cfg.Mappings[0].PathPrefix)
	}
	if cfg.Mappings[0].Group != "app" {
		t.Fatalf("group was not trimmed: %q", cfg.Mappings[0].Group)
	}
	if got := cfg.Mappings[0].WaitingPage.RefreshFrequency.Value(); got != 2*time.Second {
		t.Fatalf("unexpected refresh frequency: %s", got)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	text := strings.Replace(validConfig, "  level: info", "  level: info\n  format: json", 1)
	_, err := loadTextError(t, text, nil)
	if err == nil || !strings.Contains(err.Error(), "field format not found") {
		t.Fatalf("expected strict YAML error, got %v", err)
	}
}

func TestLoadRejectsMultipleDocuments(t *testing.T) {
	_, err := loadTextError(t, validConfig+"\n---\nversion: 1\n", nil)
	if err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Fatalf("expected multiple document error, got %v", err)
	}
}

func TestEnvironmentOverridesGlobals(t *testing.T) {
	environment := map[string]string{
		EnvPrefix + "GRPC_LISTEN_ADDRESS":     "127.0.0.1:19000",
		EnvPrefix + "ADMIN_LISTEN_ADDRESS":    "127.0.0.1:19001",
		EnvPrefix + "SABLIER_BASE_URL":        "https://sablier.example.com",
		EnvPrefix + "SABLIER_REQUEST_TIMEOUT": "7s",
		EnvPrefix + "MAX_RESPONSE_BYTES":      "2MiB",
		EnvPrefix + "SESSION_DURATION":        "45m",
		EnvPrefix + "FAILURE_POLICY":          "open",
		EnvPrefix + "RETRY_AFTER":             "9s",
		EnvPrefix + "LOG_LEVEL":               "debug",
	}
	lookup := func(key string) (string, bool) {
		value, ok := environment[key]
		return value, ok
	}
	cfg := loadText(t, validConfig, lookup)

	if cfg.Server.GRPCListenAddress != "127.0.0.1:19000" || cfg.Server.AdminListenAddress != "127.0.0.1:19001" {
		t.Fatalf("server overrides not applied: %+v", cfg.Server)
	}
	if cfg.Sablier.BaseURL != "https://sablier.example.com" || cfg.Sablier.RequestTimeout.Value() != 7*time.Second {
		t.Fatalf("Sablier overrides not applied: %+v", cfg.Sablier)
	}
	if cfg.Sablier.MaxResponseBytes.Value() != 2*1024*1024 || cfg.Sablier.SessionDuration.Value() != 45*time.Minute {
		t.Fatalf("size or session override not applied: %+v", cfg.Sablier)
	}
	if cfg.Sablier.FailurePolicy != FailurePolicyOpen || cfg.Sablier.RetryAfter.Value() != 9*time.Second || cfg.Logging.Level != "debug" {
		t.Fatalf("remaining overrides not applied: %+v %+v", cfg.Sablier, cfg.Logging)
	}
}

func TestInvalidEnvironmentValueNamesVariable(t *testing.T) {
	lookup := func(key string) (string, bool) {
		if key == EnvPrefix+"SABLIER_REQUEST_TIMEOUT" {
			return "eventually", true
		}
		return "", false
	}
	_, err := loadTextError(t, validConfig, lookup)
	if err == nil || !strings.Contains(err.Error(), EnvPrefix+"SABLIER_REQUEST_TIMEOUT") {
		t.Fatalf("expected named environment error, got %v", err)
	}
}

func TestResolvePath(t *testing.T) {
	lookup := func(key string) (string, bool) {
		if key == EnvPrefix+"CONFIG_FILE" {
			return "/etc/sablier-extproc/config.yaml", true
		}
		return "", false
	}
	if got := ResolvePath("config.yaml", lookup); got != "/etc/sablier-extproc/config.yaml" {
		t.Fatalf("unexpected resolved path: %q", got)
	}
}

func TestResolvePathFallsBackToDefault(t *testing.T) {
	t.Setenv(EnvPrefix+"CONFIG_FILE", "")
	if got := ResolvePath("config.yaml", nil); got != "config.yaml" {
		t.Fatalf("ResolvePath() = %q; want default path", got)
	}
	lookup := func(string) (string, bool) { return "   ", true }
	if got := ResolvePath("config.yaml", lookup); got != "config.yaml" {
		t.Fatalf("ResolvePath() = %q for blank override; want default path", got)
	}
}

func TestLoadUsesProcessEnvironment(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(file, []byte(validConfig), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv(EnvPrefix+"LOG_LEVEL", "warn")
	cfg, err := Load(file)
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.Logging.Level != "warn" {
		t.Fatalf("logging level = %q; want warn", cfg.Logging.Level)
	}
}

func TestLoadReportsReadError(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if err == nil || !strings.Contains(err.Error(), "read config") {
		t.Fatalf("expected read error, got %v", err)
	}
}

func TestLoadRejectsOversizedConfig(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(file, []byte(strings.Repeat("x", maxConfigFileBytes+1)), 0o600); err != nil {
		t.Fatalf("write oversized config: %v", err)
	}
	_, err := Load(file)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected size limit error, got %v", err)
	}
}

func loadText(t *testing.T, contents string, lookup LookupEnv) Config {
	t.Helper()
	cfg, err := loadTextError(t, contents, lookup)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}

func loadTextError(t *testing.T, contents string, lookup LookupEnv) (Config, error) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(file, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return LoadWithEnv(file, lookup)
}
