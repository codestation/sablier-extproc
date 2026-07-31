package config

import "time"

const (
	// CurrentVersion is the supported configuration schema version.
	CurrentVersion = 1

	// MaxSablierResponseBytes bounds the waiting page retained per request.
	MaxSablierResponseBytes ByteSize = 16 * 1024 * 1024

	// FailurePolicyOpen continues requests when Sablier fails.
	FailurePolicyOpen FailurePolicy = "open"
	// FailurePolicyClosed rejects requests when Sablier fails.
	FailurePolicyClosed FailurePolicy = "closed"

	// ExclusionExact matches one exact request path.
	ExclusionExact ExclusionType = "exact"
	// ExclusionPathPrefix matches a request path prefix.
	ExclusionPathPrefix ExclusionType = "pathPrefix"
)

// Config is the complete service configuration.
type Config struct {
	Version  int           `yaml:"version"`
	Server   ServerConfig  `yaml:"server"`
	Sablier  SablierConfig `yaml:"sablier"`
	Logging  LoggingConfig `yaml:"logging"`
	Mappings []Mapping     `yaml:"mappings"`
}

// ServerConfig configures the gRPC and administration listeners.
type ServerConfig struct {
	GRPCListenAddress  string `yaml:"grpcListenAddress"`
	AdminListenAddress string `yaml:"adminListenAddress"`
}

// SablierConfig configures requests to the Sablier API.
type SablierConfig struct {
	BaseURL          string        `yaml:"baseURL"`
	RequestTimeout   Duration      `yaml:"requestTimeout"`
	MaxResponseBytes ByteSize      `yaml:"maxResponseBytes"`
	SessionDuration  Duration      `yaml:"sessionDuration"`
	FailurePolicy    FailurePolicy `yaml:"failurePolicy"`
	RetryAfter       Duration      `yaml:"retryAfter"`
}

// LoggingConfig configures structured logging.
type LoggingConfig struct {
	Level string `yaml:"level"`
}

// Mapping associates a request host and path with a Sablier group.
type Mapping struct {
	Host            string         `yaml:"host"`
	PathPrefix      string         `yaml:"pathPrefix"`
	Group           string         `yaml:"group"`
	Exclusions      []Exclusion    `yaml:"exclusions"`
	WaitingPage     WaitingPage    `yaml:"waitingPage"`
	SessionDuration *Duration      `yaml:"sessionDuration"`
	FailurePolicy   *FailurePolicy `yaml:"failurePolicy"`
}

// WaitingPage customizes Sablier's waiting page.
type WaitingPage struct {
	DisplayName      string    `yaml:"displayName"`
	Theme            string    `yaml:"theme"`
	ShowDetails      *bool     `yaml:"showDetails"`
	RefreshFrequency *Duration `yaml:"refreshFrequency"`
}

// Exclusion identifies a path that bypasses Sablier processing.
type Exclusion struct {
	Type ExclusionType `yaml:"type"`
	Path string        `yaml:"path"`
}

// FailurePolicy controls behavior when Sablier cannot be reached.
type FailurePolicy string

// ExclusionType specifies how an exclusion path is matched.
type ExclusionType string

// Duration wraps time.Duration for configuration decoding.
type Duration time.Duration

// Value returns the underlying time.Duration.
func (d Duration) Value() time.Duration {
	return time.Duration(d)
}

// ByteSize represents a number of bytes.
type ByteSize int64

// Value returns the byte count.
func (b ByteSize) Value() int64 {
	return int64(b)
}

func defaults() Config {
	return Config{
		Version: CurrentVersion,
		Server: ServerConfig{
			GRPCListenAddress:  ":18080",
			AdminListenAddress: ":8080",
		},
		Sablier: SablierConfig{
			RequestTimeout:   Duration(3 * time.Second),
			MaxResponseBytes: ByteSize(1024 * 1024),
			SessionDuration:  Duration(time.Hour),
			FailurePolicy:    FailurePolicyClosed,
			RetryAfter:       Duration(5 * time.Second),
		},
		Logging: LoggingConfig{Level: "info"},
	}
}
