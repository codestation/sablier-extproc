package config

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// UnmarshalYAML parses a YAML duration string.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return errors.New("duration must be a string")
	}

	parsed, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", node.Value, err)
	}
	*d = Duration(parsed)
	return nil
}

// UnmarshalYAML parses a YAML byte size string or integer.
func (b *ByteSize) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return errors.New("byte size must be a string or integer")
	}

	parsed, err := parseByteSize(node.Value)
	if err != nil {
		return err
	}
	*b = parsed
	return nil
}

func parseByteSize(value string) (ByteSize, error) {
	trimmed := strings.TrimSpace(value)
	units := []struct {
		suffix     string
		multiplier int64
	}{
		{"GiB", 1024 * 1024 * 1024},
		{"MiB", 1024 * 1024},
		{"KiB", 1024},
		{"GB", 1000 * 1000 * 1000},
		{"MB", 1000 * 1000},
		{"KB", 1000},
		{"B", 1},
	}

	for _, unit := range units {
		if !strings.HasSuffix(trimmed, unit.suffix) {
			continue
		}
		number := strings.TrimSpace(strings.TrimSuffix(trimmed, unit.suffix))
		return parseScaledInteger(value, number, unit.multiplier)
	}

	return parseScaledInteger(value, trimmed, 1)
}

func parseScaledInteger(original, number string, multiplier int64) (ByteSize, error) {
	parsed, err := strconv.ParseInt(number, 10, 64)
	if err != nil || parsed <= 0 || parsed > math.MaxInt64/multiplier {
		return 0, fmt.Errorf("invalid byte size %q", original)
	}
	return ByteSize(parsed * multiplier), nil
}
