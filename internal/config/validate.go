package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// Validate normalizes and validates the configuration.
func (cfg *Config) Validate() error {
	if cfg.Version != CurrentVersion {
		return fmt.Errorf("config version must be %d", CurrentVersion)
	}
	if err := validateListenAddress("server.grpcListenAddress", cfg.Server.GRPCListenAddress); err != nil {
		return err
	}
	if err := validateListenAddress("server.adminListenAddress", cfg.Server.AdminListenAddress); err != nil {
		return err
	}
	if err := validateSablier(&cfg.Sablier); err != nil {
		return err
	}
	cfg.Logging.Level = strings.ToLower(strings.TrimSpace(cfg.Logging.Level))
	if cfg.Logging.Level != "debug" && cfg.Logging.Level != "info" && cfg.Logging.Level != "warn" && cfg.Logging.Level != "error" {
		return errors.New("logging.level must be one of debug, info, warn, error")
	}
	if len(cfg.Mappings) == 0 {
		return errors.New("at least one mapping is required")
	}

	seen := make(map[string]int, len(cfg.Mappings))
	for i := range cfg.Mappings {
		mapping := &cfg.Mappings[i]
		if err := validateMapping(mapping); err != nil {
			return fmt.Errorf("mappings[%d]: %w", i, err)
		}
		key := mapping.Host + "\x00" + mapping.PathPrefix
		if previous, ok := seen[key]; ok {
			return fmt.Errorf("mappings[%d]: ambiguous with mappings[%d] for host %q and pathPrefix %q", i, previous, mapping.Host, mapping.PathPrefix)
		}
		seen[key] = i
	}
	return nil
}

func validateListenAddress(field, address string) error {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%s must be a host:port address: %w", field, err)
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return fmt.Errorf("%s port must be between 1 and 65535", field)
	}
	return nil
}

func validateSablier(cfg *SablierConfig) error {
	parsed, err := url.ParseRequestURI(cfg.BaseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return errors.New("sablier.baseURL must be an absolute HTTP(S) URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return errors.New("sablier.baseURL must not contain credentials, path, query, or fragment")
	}
	if cfg.RequestTimeout.Value() <= 0 {
		return errors.New("sablier.requestTimeout must be positive")
	}
	if cfg.MaxResponseBytes.Value() <= 0 {
		return errors.New("sablier.maxResponseBytes must be positive")
	}
	if cfg.MaxResponseBytes > MaxSablierResponseBytes {
		return fmt.Errorf("sablier.maxResponseBytes must not exceed %d bytes", MaxSablierResponseBytes)
	}
	if cfg.SessionDuration.Value() <= 0 {
		return errors.New("sablier.sessionDuration must be positive")
	}
	if cfg.RetryAfter.Value() <= 0 {
		return errors.New("sablier.retryAfter must be positive")
	}
	if !cfg.FailurePolicy.valid() {
		return errors.New("sablier.failurePolicy must be open or closed")
	}
	return nil
}

func validateMapping(mapping *Mapping) error {
	host, err := normalizeConfiguredHost(mapping.Host)
	if err != nil {
		return err
	}
	mapping.Host = host

	if mapping.PathPrefix == "" {
		mapping.PathPrefix = "/"
	}
	if err := validatePath(mapping.PathPrefix); err != nil {
		return fmt.Errorf("pathPrefix: %w", err)
	}
	if strings.TrimSpace(mapping.Group) == "" {
		return errors.New("group is required")
	}
	mapping.Group = strings.TrimSpace(mapping.Group)
	if mapping.SessionDuration != nil && mapping.SessionDuration.Value() <= 0 {
		return errors.New("sessionDuration must be positive")
	}
	if mapping.FailurePolicy != nil && !mapping.FailurePolicy.valid() {
		return errors.New("failurePolicy must be open or closed")
	}
	if mapping.WaitingPage.RefreshFrequency != nil && mapping.WaitingPage.RefreshFrequency.Value() <= 0 {
		return errors.New("waitingPage.refreshFrequency must be positive")
	}

	seen := make(map[string]int, len(mapping.Exclusions))
	for i := range mapping.Exclusions {
		exclusion := &mapping.Exclusions[i]
		if exclusion.Type != ExclusionExact && exclusion.Type != ExclusionPathPrefix {
			return fmt.Errorf("exclusions[%d].type must be exact or pathPrefix", i)
		}
		if err := validatePath(exclusion.Path); err != nil {
			return fmt.Errorf("exclusions[%d].path: %w", i, err)
		}
		key := string(exclusion.Type) + "\x00" + exclusion.Path
		if previous, ok := seen[key]; ok {
			return fmt.Errorf("exclusions[%d] duplicates exclusions[%d]", i, previous)
		}
		seen[key] = i
	}
	return nil
}

func normalizeConfiguredHost(value string) (string, error) {
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(value), "."))
	if host == "" {
		return "", errors.New("host is required")
	}
	if strings.HasPrefix(host, "*.") {
		if strings.Contains(host[2:], "*") || !validDNSName(host[2:]) {
			return "", fmt.Errorf("host %q is not a valid wildcard hostname", value)
		}
		return host, nil
	}
	if strings.Contains(host, "*") || !validDNSName(host) {
		return "", fmt.Errorf("host %q is not a valid hostname", value)
	}
	return host, nil
}

func validDNSName(host string) bool {
	if len(host) > 253 {
		return false
	}
	labels := strings.SplitSeq(host, ".")
	for label := range labels {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return false
			}
		}
	}
	return true
}

func validatePath(value string) error {
	if !strings.HasPrefix(value, "/") || strings.ContainsAny(value, "?#") {
		return errors.New("must be an absolute path without query or fragment")
	}
	if path.Clean(value) != value {
		return errors.New("must be normalized without duplicate separators, dot segments, or trailing slash")
	}
	return nil
}

func (p FailurePolicy) valid() bool {
	return p == FailurePolicyOpen || p == FailurePolicyClosed
}
