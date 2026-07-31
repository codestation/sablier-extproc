package config

import (
	"strings"
	"testing"
	"time"
)

func TestValidationFailures(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		message string
	}{
		{"version", func(cfg *Config) { cfg.Version = 2 }, "config version"},
		{"gRPC address", func(cfg *Config) { cfg.Server.GRPCListenAddress = "missing-port" }, "grpcListenAddress"},
		{"base URL", func(cfg *Config) { cfg.Sablier.BaseURL = "ftp://sablier" }, "baseURL"},
		{"base URL path", func(cfg *Config) { cfg.Sablier.BaseURL = "http://sablier/api" }, "must not contain"},
		{"timeout", func(cfg *Config) { cfg.Sablier.RequestTimeout = 0 }, "requestTimeout"},
		{"response limit", func(cfg *Config) { cfg.Sablier.MaxResponseBytes = MaxSablierResponseBytes + 1 }, "maxResponseBytes"},
		{"policy", func(cfg *Config) { cfg.Sablier.FailurePolicy = "sometimes" }, "failurePolicy"},
		{"log level", func(cfg *Config) { cfg.Logging.Level = "trace" }, "logging.level"},
		{"hostname", func(cfg *Config) { cfg.Mappings[0].Host = "bad_*_host" }, "valid hostname"},
		{"wildcard", func(cfg *Config) { cfg.Mappings[0].Host = "api.*.example.com" }, "valid hostname"},
		{"path", func(cfg *Config) { cfg.Mappings[0].PathPrefix = "/api/../admin" }, "normalized"},
		{"group", func(cfg *Config) { cfg.Mappings[0].Group = " " }, "group is required"},
		{"mapping duration", func(cfg *Config) { value := Duration(-time.Second); cfg.Mappings[0].SessionDuration = &value }, "sessionDuration"},
		{"exclusion type", func(cfg *Config) { cfg.Mappings[0].Exclusions[0].Type = "regex" }, "exclusions[0].type"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := validObject()
			test.mutate(&cfg)
			err := cfg.Validate()
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("expected error containing %q, got %v", test.message, err)
			}
		})
	}
}

func TestDuplicateMappingsAreRejectedAfterNormalization(t *testing.T) {
	cfg := validObject()
	cfg.Mappings = append(cfg.Mappings, Mapping{Host: "APP.EXAMPLE.COM.", PathPrefix: "/", Group: "other"})
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguous mapping error, got %v", err)
	}
}

func TestGatewayStyleWildcardIsAccepted(t *testing.T) {
	cfg := validObject()
	cfg.Mappings[0].Host = "*.Preview.Example.com."
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate wildcard: %v", err)
	}
	if cfg.Mappings[0].Host != "*.preview.example.com" {
		t.Fatalf("wildcard not normalized: %q", cfg.Mappings[0].Host)
	}
}

func TestLogLevelIsNormalized(t *testing.T) {
	cfg := validObject()
	cfg.Logging.Level = " WARN "
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate log level: %v", err)
	}
	if cfg.Logging.Level != "warn" {
		t.Fatalf("logging level = %q; want warn", cfg.Logging.Level)
	}
}

func TestByteSizeParsing(t *testing.T) {
	tests := map[string]int64{
		"1":    1,
		"1KiB": 1024,
		"2MiB": 2 * 1024 * 1024,
		"3MB":  3 * 1000 * 1000,
	}
	for input, want := range tests {
		got, err := parseByteSize(input)
		if err != nil || got.Value() != want {
			t.Fatalf("parseByteSize(%q) = %d, %v; want %d", input, got.Value(), err, want)
		}
	}
	for _, input := range []string{"", "0", "-1MiB", "1.5MiB", "lots"} {
		if _, err := parseByteSize(input); err == nil {
			t.Fatalf("parseByteSize(%q) unexpectedly succeeded", input)
		}
	}
}

func validObject() Config {
	cfg := defaults()
	cfg.Sablier.BaseURL = "http://sablier:10000"
	cfg.Mappings = []Mapping{{
		Host:       "app.example.com",
		PathPrefix: "/",
		Group:      "app",
		Exclusions: []Exclusion{{Type: ExclusionExact, Path: "/healthz"}},
	}}
	return cfg
}
