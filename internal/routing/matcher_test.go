package routing

import (
	"testing"

	"github.com/sablierapp/sablier-extproc/internal/config"
)

func TestMatchPrecedence(t *testing.T) {
	matcher := New([]config.Mapping{
		{Host: "*.example.com", PathPrefix: "/api", Group: "wildcard-api"},
		{Host: "*.preview.example.com", PathPrefix: "/", Group: "preview"},
		{Host: "app.example.com", PathPrefix: "/", Group: "exact-root"},
		{Host: "app.example.com", PathPrefix: "/admin", Group: "exact-admin"},
	})

	tests := []struct {
		name      string
		authority string
		path      string
		group     string
	}{
		{"exact before wildcard", "APP.EXAMPLE.COM.:443", "/api/users", "exact-root"},
		{"longest exact path", "app.example.com", "/admin/users", "exact-admin"},
		{"specific wildcard", "foo.bar.preview.example.com", "/api/users", "preview"},
		{"general wildcard", "foo.example.com", "/api?active=true", "wildcard-api"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := matcher.Match(test.authority, test.path)
			if err != nil {
				t.Fatalf("match: %v", err)
			}
			if result.Mapping == nil || result.Mapping.Group != test.group {
				t.Fatalf("unexpected result: %+v", result)
			}
		})
	}
}

func TestWildcardRequiresAdditionalLabel(t *testing.T) {
	matcher := New([]config.Mapping{{Host: "*.example.com", PathPrefix: "/", Group: "wildcard"}})

	result, err := matcher.Match("example.com", "/")
	if err != nil {
		t.Fatalf("match: %v", err)
	}
	if result.Mapping != nil {
		t.Fatalf("wildcard matched its bare suffix: %+v", result)
	}

	result, err = matcher.Match("deep.sub.example.com", "/")
	if err != nil || result.Mapping == nil {
		t.Fatalf("wildcard did not match multiple labels: %+v, %v", result, err)
	}
}

func TestPathPrefixUsesSegmentBoundaries(t *testing.T) {
	matcher := New([]config.Mapping{{Host: "app.example.com", PathPrefix: "/api", Group: "api"}})

	for _, requestPath := range []string{"/api", "/api/v1"} {
		result, err := matcher.Match("app.example.com", requestPath)
		if err != nil || result.Mapping == nil {
			t.Fatalf("expected %q to match: %+v, %v", requestPath, result, err)
		}
	}
	result, err := matcher.Match("app.example.com", "/apiculture")
	if err != nil || result.Mapping != nil {
		t.Fatalf("segment prefix overmatched: %+v, %v", result, err)
	}
}

func TestExclusionsApplyAfterMapping(t *testing.T) {
	matcher := New([]config.Mapping{{
		Host:       "app.example.com",
		PathPrefix: "/",
		Group:      "app",
		Exclusions: []config.Exclusion{
			{Type: config.ExclusionExact, Path: "/healthz"},
			{Type: config.ExclusionPathPrefix, Path: "/metrics"},
		},
	}})

	tests := []struct {
		path   string
		bypass bool
	}{
		{"/healthz", true},
		{"/healthz/deep", false},
		{"/metrics", true},
		{"/metrics/process", true},
		{"/metrics-old", false},
		{"/", false},
	}
	for _, test := range tests {
		result, err := matcher.Match("app.example.com", test.path)
		if err != nil || result.Mapping == nil || result.Bypass != test.bypass {
			t.Fatalf("match %q = %+v, %v; want bypass=%t", test.path, result, err, test.bypass)
		}
	}
}

func TestUnmatchedAndInvalidRequests(t *testing.T) {
	matcher := New([]config.Mapping{{Host: "app.example.com", PathPrefix: "/", Group: "app"}})

	result, err := matcher.Match("other.example.com", "/")
	if err != nil || result.Mapping != nil {
		t.Fatalf("unexpected unmatched result: %+v, %v", result, err)
	}
	for _, test := range []struct {
		authority string
		path      string
	}{
		{"", "/"},
		{"app.example.com:not-a-port", "/"},
		{"app.example.com", "not-absolute"},
	} {
		if _, err := matcher.Match(test.authority, test.path); err == nil {
			t.Fatalf("expected invalid request error for %+v", test)
		}
	}
}

func TestMatcherOwnsMappingSlice(t *testing.T) {
	duration := config.Duration(5)
	showDetails := true
	mappings := []config.Mapping{{
		Host:            "app.example.com",
		PathPrefix:      "/",
		Group:           "app",
		SessionDuration: &duration,
		Exclusions:      []config.Exclusion{{Type: config.ExclusionExact, Path: "/healthz"}},
		WaitingPage:     config.WaitingPage{ShowDetails: &showDetails},
	}}
	matcher := New(mappings)
	mappings[0].Group = "changed"
	mappings[0].Exclusions[0].Path = "/changed"
	*mappings[0].SessionDuration = 10
	*mappings[0].WaitingPage.ShowDetails = false

	result, err := matcher.Match("app.example.com", "/healthz")
	if err != nil || result.Mapping == nil || result.Mapping.Group != "app" || !result.Bypass {
		t.Fatalf("matcher retained caller mutation: %+v, %v", result, err)
	}
	if result.Mapping.SessionDuration.Value() != 5 || !*result.Mapping.WaitingPage.ShowDetails {
		t.Fatalf("matcher retained nested pointer mutation: %+v", result.Mapping)
	}
}
