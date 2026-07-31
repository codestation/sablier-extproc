// Package sablier provides a bounded HTTP client for Sablier's dynamic strategy API.
package sablier

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sablierapp/sablier-extproc/internal/config"
)

const (
	// SessionStatusHeader contains Sablier's session readiness status.
	SessionStatusHeader = "X-Sablier-Session-Status"

	// StatusReady indicates that the requested group is ready.
	StatusReady Status = "ready"
	// StatusNotReady indicates that the requested group is still starting.
	StatusNotReady Status = "not-ready"

	dynamicStrategyPath       = "/api/strategies/dynamic"
	maxResponseHeaderBytes    = 64 * 1024
	maxIdleConnectionsPerHost = 32
)

// ErrResponseTooLarge indicates that Sablier exceeded the configured response limit.
var ErrResponseTooLarge = errors.New("sablier response exceeds configured limit")

// Status is a Sablier session readiness status.
type Status string

// Request contains parameters for a dynamic strategy request.
type Request struct {
	Group            string
	SessionDuration  time.Duration
	DisplayName      string
	Theme            string
	ShowDetails      *bool
	RefreshFrequency *time.Duration
}

// Response contains Sablier's validated response.
type Response struct {
	Status       Status
	Body         []byte
	ContentType  string
	CacheControl string
}

// HTTPStatusError reports a non-successful Sablier HTTP status.
type HTTPStatusError struct {
	StatusCode int
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("sablier returned HTTP status %d", e.StatusCode)
}

// SessionStatusError reports a missing or invalid Sablier session status.
type SessionStatusError struct {
	Value string
}

func (e *SessionStatusError) Error() string {
	if e.Value == "" {
		return "sablier response is missing a valid session status"
	}
	return fmt.Sprintf("sablier returned invalid session status %q", e.Value)
}

// Client calls Sablier's dynamic strategy API.
type Client struct {
	endpoint         *url.URL
	httpClient       *http.Client
	maxResponseBytes int64
}

// NewClient creates a Client from validated Sablier configuration.
func NewClient(cfg config.SablierConfig) (*Client, error) {
	baseURL, err := url.ParseRequestURI(cfg.BaseURL)
	if err != nil || baseURL.Host == "" || (baseURL.Scheme != "http" && baseURL.Scheme != "https") {
		return nil, errors.New("invalid sablier base URL")
	}
	if baseURL.User != nil || baseURL.RawQuery != "" || baseURL.Fragment != "" || (baseURL.Path != "" && baseURL.Path != "/") {
		return nil, errors.New("sablier base URL must not contain credentials, path, query, or fragment")
	}
	if cfg.RequestTimeout.Value() <= 0 {
		return nil, errors.New("sablier request timeout must be positive")
	}
	if cfg.MaxResponseBytes.Value() <= 0 {
		return nil, errors.New("sablier response limit must be positive")
	}
	if cfg.MaxResponseBytes > config.MaxSablierResponseBytes {
		return nil, fmt.Errorf("sablier response limit must not exceed %d bytes", config.MaxSablierResponseBytes)
	}

	endpoint := *baseURL
	endpoint.Path = strings.TrimSuffix(endpoint.Path, "/") + dynamicStrategyPath
	endpoint.RawPath = ""
	endpoint.RawQuery = ""
	endpoint.Fragment = ""

	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("default HTTP transport has an unexpected type")
	}
	transport := defaultTransport.Clone()
	transport.MaxResponseHeaderBytes = maxResponseHeaderBytes
	transport.MaxIdleConnsPerHost = maxIdleConnectionsPerHost

	return &Client{
		endpoint: &endpoint,
		httpClient: &http.Client{
			Timeout:   cfg.RequestTimeout.Value(),
			Transport: transport,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		maxResponseBytes: cfg.MaxResponseBytes.Value(),
	}, nil
}

// Request calls Sablier and returns a validated, size-bounded response.
func (c *Client) Request(ctx context.Context, request Request) (Response, error) {
	requestURL := *c.endpoint
	query := requestURL.Query()
	query.Set("group", request.Group)
	query.Set("session_duration", request.SessionDuration.String())
	if request.DisplayName != "" {
		query.Set("display_name", request.DisplayName)
	}
	if request.Theme != "" {
		query.Set("theme", request.Theme)
	}
	if request.ShowDetails != nil {
		query.Set("show_details", strconv.FormatBool(*request.ShowDetails))
	}
	if request.RefreshFrequency != nil {
		query.Set("refresh_frequency", request.RefreshFrequency.String())
	}
	requestURL.RawQuery = query.Encode()

	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), http.NoBody)
	if err != nil {
		return Response{}, fmt.Errorf("create Sablier request: %w", err)
	}
	response, err := c.httpClient.Do(httpRequest)
	if err != nil {
		return Response{}, fmt.Errorf("call Sablier: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return Response{}, closeResponseBody(response.Body, &HTTPStatusError{StatusCode: response.StatusCode})
	}
	if response.ContentLength > c.maxResponseBytes {
		return Response{}, closeResponseBody(response.Body, ErrResponseTooLarge)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, c.maxResponseBytes+1))
	if err != nil {
		closeErr := response.Body.Close()
		if closeErr != nil {
			return Response{}, errors.Join(fmt.Errorf("read Sablier response: %w", err), fmt.Errorf("close Sablier response: %w", closeErr))
		}
		return Response{}, fmt.Errorf("read Sablier response: %w", err)
	}
	if err := response.Body.Close(); err != nil {
		return Response{}, fmt.Errorf("close Sablier response: %w", err)
	}
	if int64(len(body)) > c.maxResponseBytes {
		return Response{}, ErrResponseTooLarge
	}

	statusValues := response.Header.Values(SessionStatusHeader)
	if len(statusValues) != 1 {
		return Response{}, &SessionStatusError{}
	}
	status := Status(statusValues[0])
	if status != StatusReady && status != StatusNotReady {
		return Response{}, &SessionStatusError{Value: statusValues[0]}
	}

	return Response{
		Status:       status,
		Body:         body,
		ContentType:  response.Header.Get("Content-Type"),
		CacheControl: response.Header.Get("Cache-Control"),
	}, nil
}

func closeResponseBody(body io.Closer, responseErr error) error {
	if err := body.Close(); err != nil {
		return errors.Join(responseErr, fmt.Errorf("close Sablier response: %w", err))
	}
	return responseErr
}
