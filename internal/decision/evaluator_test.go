package decision

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/sablierapp/sablier-extproc/internal/config"
	"github.com/sablierapp/sablier-extproc/internal/sablier"
)

func TestEvaluateBehaviorMatrix(t *testing.T) {
	upstreamError := errors.New("Sablier unavailable")
	tests := []struct {
		name          string
		method        string
		response      sablier.Response
		err           error
		policy        config.FailurePolicy
		action        Action
		outcome       Outcome
		statusCode    int
		body          string
		retryAfter    string
		sablierStatus string
	}{
		{
			name: "ready continues", method: http.MethodGet,
			response: sablier.Response{Status: sablier.StatusReady, Body: []byte("ignored")},
			action:   ActionContinue, outcome: OutcomeReady,
		},
		{
			name: "not ready GET serves HTML", method: http.MethodGet,
			response: sablier.Response{Status: sablier.StatusNotReady, Body: []byte("<html>wait</html>"), ContentType: "text/html", CacheControl: "no-cache"},
			action:   ActionRespond, outcome: OutcomeWaiting, statusCode: http.StatusOK, body: "<html>wait</html>", sablierStatus: "not-ready",
		},
		{
			name: "not ready HEAD omits body", method: http.MethodHead,
			response: sablier.Response{Status: sablier.StatusNotReady, Body: []byte("<html>wait</html>"), ContentType: "text/html"},
			action:   ActionRespond, outcome: OutcomeWaiting, statusCode: http.StatusOK, sablierStatus: "not-ready",
		},
		{
			name: "not ready POST is rejected", method: http.MethodPost,
			response: sablier.Response{Status: sablier.StatusNotReady, Body: []byte("ignored")},
			action:   ActionRespond, outcome: OutcomeMethodNotAllowed, statusCode: http.StatusServiceUnavailable,
			body: unavailableBody, retryAfter: "6", sablierStatus: "not-ready",
		},
		{
			name: "failure opens", method: http.MethodGet, err: upstreamError, policy: config.FailurePolicyOpen,
			action: ActionContinue, outcome: OutcomeFailureOpen,
		},
		{
			name: "failure closes", method: http.MethodGet, err: upstreamError, policy: config.FailurePolicyClosed,
			action: ActionRespond, outcome: OutcomeFailureClosed, statusCode: http.StatusServiceUnavailable,
			body: unavailableBody, retryAfter: "6",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := &fakeRequester{response: test.response, err: test.err}
			defaults := defaultSablierConfig()
			if test.policy != "" {
				defaults.FailurePolicy = test.policy
			}
			result := New(client, defaults).Evaluate(context.Background(), test.method, config.Mapping{Group: "app"})

			if client.calls != 1 {
				t.Fatalf("Sablier called %d times; want exactly once", client.calls)
			}
			if result.Action != test.action || result.Outcome != test.outcome || result.StatusCode != test.statusCode {
				t.Fatalf("unexpected decision: %+v", result)
			}
			if string(result.Body) != test.body {
				t.Fatalf("body = %q; want %q", result.Body, test.body)
			}
			if result.Headers.Get("Retry-After") != test.retryAfter {
				t.Fatalf("Retry-After = %q; want %q", result.Headers.Get("Retry-After"), test.retryAfter)
			}
			if result.Headers.Get(sablier.SessionStatusHeader) != test.sablierStatus {
				t.Fatalf("Sablier status = %q; want %q", result.Headers.Get(sablier.SessionStatusHeader), test.sablierStatus)
			}
			if test.err != nil && !errors.Is(result.Err, test.err) {
				t.Fatalf("decision did not retain upstream error: %v", result.Err)
			}
		})
	}
}

func TestEvaluateUsesMappingOverridesAndWaitingOptions(t *testing.T) {
	sessionDuration := config.Duration(30 * time.Minute)
	failurePolicy := config.FailurePolicyOpen
	showDetails := false
	refreshFrequency := config.Duration(2500 * time.Millisecond)
	client := &fakeRequester{err: errors.New("failure")}
	mapping := config.Mapping{
		Group:           "preview",
		SessionDuration: &sessionDuration,
		FailurePolicy:   &failurePolicy,
		WaitingPage: config.WaitingPage{
			DisplayName:      "Preview",
			Theme:            "matrix",
			ShowDetails:      &showDetails,
			RefreshFrequency: &refreshFrequency,
		},
	}

	result := New(client, defaultSablierConfig()).Evaluate(context.Background(), http.MethodGet, mapping)
	if result.Outcome != OutcomeFailureOpen || result.Action != ActionContinue {
		t.Fatalf("mapping failure policy override not used: %+v", result)
	}
	request := client.request
	if request.Group != "preview" || request.SessionDuration != 30*time.Minute || request.DisplayName != "Preview" || request.Theme != "matrix" {
		t.Fatalf("mapping options not translated: %+v", request)
	}
	if request.ShowDetails == nil || *request.ShowDetails || request.RefreshFrequency == nil || *request.RefreshFrequency != 2500*time.Millisecond {
		t.Fatalf("optional waiting page options not translated: %+v", request)
	}
}

func TestWaitingResponseHeaders(t *testing.T) {
	client := &fakeRequester{response: sablier.Response{
		Status:       sablier.StatusNotReady,
		Body:         []byte("waiting"),
		ContentType:  "text/html; charset=utf-8",
		CacheControl: "no-cache",
	}}
	result := New(client, defaultSablierConfig()).Evaluate(context.Background(), http.MethodGet, config.Mapping{Group: "app"})
	if result.Headers.Get("Content-Type") != "text/html; charset=utf-8" || result.Headers.Get("Cache-Control") != "no-cache" {
		t.Fatalf("Sablier response headers not preserved: %v", result.Headers)
	}
	if result.Headers.Get("Content-Length") != "7" {
		t.Fatalf("unexpected Content-Length: %q", result.Headers.Get("Content-Length"))
	}
}

type fakeRequester struct {
	response sablier.Response
	err      error
	calls    int
	request  sablier.Request
}

func (f *fakeRequester) Request(_ context.Context, request sablier.Request) (sablier.Response, error) {
	f.calls++
	f.request = request
	return f.response, f.err
}

func defaultSablierConfig() config.SablierConfig {
	return config.SablierConfig{
		SessionDuration: config.Duration(time.Hour),
		FailurePolicy:   config.FailurePolicyClosed,
		RetryAfter:      config.Duration(5500 * time.Millisecond),
	}
}
