// Package decision determines how requests proceed based on Sablier responses.
package decision

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/sablierapp/sablier-extproc/internal/config"
	"github.com/sablierapp/sablier-extproc/internal/sablier"
)

const unavailableBody = "service unavailable\n"

// Action describes how Envoy should handle a request.
type Action string

const (
	// ActionContinue lets Envoy continue processing the request.
	ActionContinue Action = "continue"
	// ActionRespond sends an immediate response through Envoy.
	ActionRespond Action = "respond"
)

// Outcome identifies the result of evaluating a request.
type Outcome string

const (
	// OutcomeReady indicates that the target group is ready.
	OutcomeReady Outcome = "ready"
	// OutcomeWaiting indicates that a waiting page should be returned.
	OutcomeWaiting Outcome = "waiting"
	// OutcomeMethodNotAllowed indicates that a non-idempotent request cannot wait.
	OutcomeMethodNotAllowed Outcome = "not_ready_method"
	// OutcomeFailureOpen indicates that a Sablier failure was allowed through.
	OutcomeFailureOpen Outcome = "failure_open"
	// OutcomeFailureClosed indicates that a Sablier failure was rejected.
	OutcomeFailureClosed Outcome = "failure_closed"
)

// Result contains the action and response produced by an evaluation.
type Result struct {
	Action     Action
	Outcome    Outcome
	StatusCode int
	Headers    http.Header
	Body       []byte
	Err        error
}

// Requester queries Sablier for a group status.
type Requester interface {
	Request(context.Context, sablier.Request) (sablier.Response, error)
}

// Evaluator turns Sablier responses into proxy decisions.
type Evaluator struct {
	client   Requester
	defaults config.SablierConfig
}

// New creates an Evaluator.
func New(client Requester, defaults config.SablierConfig) *Evaluator {
	return &Evaluator{client: client, defaults: defaults}
}

// Evaluate evaluates a request for the supplied mapping.
func (e *Evaluator) Evaluate(ctx context.Context, method string, mapping config.Mapping) Result {
	sessionDuration := e.defaults.SessionDuration.Value()
	if mapping.SessionDuration != nil {
		sessionDuration = mapping.SessionDuration.Value()
	}
	failurePolicy := e.defaults.FailurePolicy
	if mapping.FailurePolicy != nil {
		failurePolicy = *mapping.FailurePolicy
	}

	request := sablier.Request{
		Group:           mapping.Group,
		SessionDuration: sessionDuration,
		DisplayName:     mapping.WaitingPage.DisplayName,
		Theme:           mapping.WaitingPage.Theme,
		ShowDetails:     mapping.WaitingPage.ShowDetails,
	}
	if mapping.WaitingPage.RefreshFrequency != nil {
		value := mapping.WaitingPage.RefreshFrequency.Value()
		request.RefreshFrequency = &value
	}

	response, err := e.client.Request(ctx, request)
	if err != nil {
		if failurePolicy == config.FailurePolicyOpen {
			return Result{Action: ActionContinue, Outcome: OutcomeFailureOpen, Err: err}
		}
		return unavailable(OutcomeFailureClosed, e.defaults.RetryAfter.Value(), err, false)
	}

	if response.Status == sablier.StatusReady {
		return Result{Action: ActionContinue, Outcome: OutcomeReady}
	}

	switch method {
	case http.MethodGet:
		headers := waitingHeaders(response, len(response.Body))
		return Result{
			Action:     ActionRespond,
			Outcome:    OutcomeWaiting,
			StatusCode: http.StatusOK,
			Headers:    headers,
			Body:       response.Body,
		}
	case http.MethodHead:
		headers := waitingHeaders(response, len(response.Body))
		return Result{
			Action:     ActionRespond,
			Outcome:    OutcomeWaiting,
			StatusCode: http.StatusOK,
			Headers:    headers,
		}
	default:
		return unavailable(OutcomeMethodNotAllowed, e.defaults.RetryAfter.Value(), nil, true)
	}
}

func waitingHeaders(response sablier.Response, contentLength int) http.Header {
	headers := make(http.Header)
	headers.Set(sablier.SessionStatusHeader, string(response.Status))
	headers.Set("Content-Length", strconv.Itoa(contentLength))
	if response.ContentType != "" {
		headers.Set("Content-Type", response.ContentType)
	}
	if response.CacheControl != "" {
		headers.Set("Cache-Control", response.CacheControl)
	}
	return headers
}

func unavailable(outcome Outcome, retryAfter time.Duration, err error, notReady bool) Result {
	headers := make(http.Header)
	headers.Set("Cache-Control", "no-store")
	headers.Set("Content-Type", "text/plain; charset=utf-8")
	headers.Set("Content-Length", strconv.Itoa(len(unavailableBody)))
	headers.Set("Retry-After", strconv.FormatInt(retryAfterSeconds(retryAfter), 10))
	if notReady {
		headers.Set(sablier.SessionStatusHeader, string(sablier.StatusNotReady))
	}
	return Result{
		Action:     ActionRespond,
		Outcome:    outcome,
		StatusCode: http.StatusServiceUnavailable,
		Headers:    headers,
		Body:       []byte(unavailableBody),
		Err:        err,
	}
}

func retryAfterSeconds(duration time.Duration) int64 {
	seconds := int64(duration / time.Second)
	if duration%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		return 1
	}
	return seconds
}
