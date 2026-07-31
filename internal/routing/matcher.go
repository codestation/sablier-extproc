// Package routing matches incoming requests to configured Sablier groups.
package routing

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/sablierapp/sablier-extproc/internal/config"
)

// Matcher matches request authorities and paths against configured mappings.
type Matcher struct {
	mappings []config.Mapping
}

// Result describes the selected mapping and whether it should be bypassed.
type Result struct {
	Mapping *config.Mapping
	Bypass  bool
}

// New creates a Matcher with an owned copy of the mappings.
func New(mappings []config.Mapping) *Matcher {
	owned := make([]config.Mapping, len(mappings))
	for i := range mappings {
		owned[i] = cloneMapping(mappings[i])
	}
	return &Matcher{mappings: owned}
}

func cloneMapping(mapping config.Mapping) config.Mapping {
	mapping.Exclusions = slices.Clone(mapping.Exclusions)
	mapping.SessionDuration = clonePointer(mapping.SessionDuration)
	mapping.FailurePolicy = clonePointer(mapping.FailurePolicy)
	mapping.WaitingPage.ShowDetails = clonePointer(mapping.WaitingPage.ShowDetails)
	mapping.WaitingPage.RefreshFrequency = clonePointer(mapping.WaitingPage.RefreshFrequency)
	return mapping
}

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	owned := *value
	return &owned
}

// Match selects the most specific mapping for a request.
func (m *Matcher) Match(authority, requestTarget string) (Result, error) {
	host, err := normalizeAuthority(authority)
	if err != nil {
		return Result{}, err
	}
	requestURL, err := url.ParseRequestURI(requestTarget)
	if err != nil || requestURL.Path == "" || !strings.HasPrefix(requestURL.Path, "/") {
		return Result{}, fmt.Errorf("invalid request target %q", requestTarget)
	}

	best := -1
	bestExact := false
	bestHostLength := -1
	bestPathLength := -1
	for i := range m.mappings {
		exact, hostLength, matches := matchHost(m.mappings[i].Host, host)
		if !matches || !pathPrefixMatches(m.mappings[i].PathPrefix, requestURL.Path) {
			continue
		}
		pathLength := len(m.mappings[i].PathPrefix)
		if best == -1 || moreSpecific(exact, hostLength, pathLength, bestExact, bestHostLength, bestPathLength) {
			best = i
			bestExact = exact
			bestHostLength = hostLength
			bestPathLength = pathLength
		}
	}
	if best == -1 {
		return Result{}, nil
	}

	mapping := &m.mappings[best]
	return Result{Mapping: mapping, Bypass: excluded(mapping.Exclusions, requestURL.Path)}, nil
}

func normalizeAuthority(authority string) (string, error) {
	value := strings.TrimSpace(authority)
	if value == "" {
		return "", errors.New("authority is empty")
	}

	host := value
	if splitHost, port, err := net.SplitHostPort(value); err == nil {
		if _, parseErr := strconv.ParseUint(port, 10, 16); parseErr != nil {
			return "", fmt.Errorf("invalid authority %q", authority)
		}
		host = splitHost
	} else if strings.Count(value, ":") == 1 {
		candidate, port, found := strings.Cut(value, ":")
		if found {
			if _, parseErr := strconv.ParseUint(port, 10, 16); parseErr != nil {
				return "", fmt.Errorf("invalid authority %q", authority)
			}
			host = candidate
		}
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "" || strings.ContainsAny(host, "/*") {
		return "", fmt.Errorf("invalid authority %q", authority)
	}
	return host, nil
}

func matchHost(pattern, host string) (exact bool, specificity int, matches bool) {
	if !strings.HasPrefix(pattern, "*.") {
		return true, len(pattern), pattern == host
	}
	suffix := pattern[2:]
	return false, len(suffix), host != suffix && strings.HasSuffix(host, "."+suffix)
}

func moreSpecific(exact bool, hostLength, pathLength int, currentExact bool, currentHostLength, currentPathLength int) bool {
	if exact != currentExact {
		return exact
	}
	if hostLength != currentHostLength {
		return hostLength > currentHostLength
	}
	return pathLength > currentPathLength
}

func pathPrefixMatches(prefix, requestPath string) bool {
	if prefix == "/" || requestPath == prefix {
		return true
	}
	return strings.HasPrefix(requestPath, prefix+"/")
}

func excluded(exclusions []config.Exclusion, requestPath string) bool {
	for _, exclusion := range exclusions {
		switch exclusion.Type {
		case config.ExclusionExact:
			if requestPath == exclusion.Path {
				return true
			}
		case config.ExclusionPathPrefix:
			if pathPrefixMatches(exclusion.Path, requestPath) {
				return true
			}
		}
	}
	return false
}
