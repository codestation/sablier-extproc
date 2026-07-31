package sablier

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sablierapp/sablier-extproc/internal/config"
)

func TestClientBuildsDynamicStrategyRequest(t *testing.T) {
	showDetails := false
	refreshFrequency := 7 * time.Second
	var requestSeen *http.Request
	client := newTestClient(t, time.Second, 1024, func(request *http.Request) (*http.Response, error) {
		requestSeen = request.Clone(request.Context())
		headers := make(http.Header)
		headers.Set(SessionStatusHeader, string(StatusNotReady))
		headers.Set("Content-Type", "text/html; charset=utf-8")
		headers.Set("Cache-Control", "no-cache")
		return httpResponse(http.StatusOK, headers, "<html>waiting</html>"), nil
	})
	response, err := client.Request(context.Background(), Request{
		Group:            "preview api",
		SessionDuration:  30 * time.Minute,
		DisplayName:      "Preview API",
		Theme:            "hacker-terminal",
		ShowDetails:      &showDetails,
		RefreshFrequency: &refreshFrequency,
	})
	if err != nil {
		t.Fatalf("request Sablier: %v", err)
	}

	seen := requestSeen
	if seen.Method != http.MethodGet || seen.URL.Path != dynamicStrategyPath {
		t.Fatalf("unexpected request: %s %s", seen.Method, seen.URL.Path)
	}
	wantQuery := url.Values{
		"group":             {"preview api"},
		"session_duration":  {"30m0s"},
		"display_name":      {"Preview API"},
		"theme":             {"hacker-terminal"},
		"show_details":      {"false"},
		"refresh_frequency": {"7s"},
	}
	if seen.URL.Query().Encode() != wantQuery.Encode() {
		t.Fatalf("unexpected query: %s", seen.URL.RawQuery)
	}
	if response.Status != StatusNotReady || string(response.Body) != "<html>waiting</html>" {
		t.Fatalf("unexpected response: %+v", response)
	}
	if response.ContentType != "text/html; charset=utf-8" || response.CacheControl != "no-cache" {
		t.Fatalf("response headers not captured: %+v", response)
	}
}

func TestClientOmitsUnsetWaitingPageOptions(t *testing.T) {
	var requestSeen *http.Request
	client := newTestClient(t, time.Second, 1024, func(request *http.Request) (*http.Response, error) {
		requestSeen = request.Clone(request.Context())
		headers := make(http.Header)
		headers.Set(SessionStatusHeader, string(StatusReady))
		return httpResponse(http.StatusOK, headers, ""), nil
	})
	_, err := client.Request(context.Background(), Request{Group: "app", SessionDuration: time.Hour})
	if err != nil {
		t.Fatalf("request Sablier: %v", err)
	}
	query := requestSeen.URL.Query()
	for _, key := range []string{"display_name", "theme", "show_details", "refresh_frequency"} {
		if query.Has(key) {
			t.Fatalf("unset option %q was sent", key)
		}
	}
}

func TestClientRejectsHTTPAndSessionStatusFailures(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		header     []string
		check      func(error) bool
	}{
		{
			name:       "HTTP status",
			statusCode: http.StatusBadGateway,
			check: func(err error) bool {
				var statusErr *HTTPStatusError
				return errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusBadGateway
			},
		},
		{name: "missing status", statusCode: http.StatusOK, check: func(err error) bool {
			var statusErr *SessionStatusError
			return errors.As(err, &statusErr)
		}},
		{name: "invalid status", statusCode: http.StatusOK, header: []string{"starting"}, check: func(err error) bool {
			var statusErr *SessionStatusError
			return errors.As(err, &statusErr) && statusErr.Value == "starting"
		}},
		{name: "duplicate status", statusCode: http.StatusOK, header: []string{"ready", "not-ready"}, check: func(err error) bool {
			var statusErr *SessionStatusError
			return errors.As(err, &statusErr)
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := newTestClient(t, time.Second, 1024, func(_ *http.Request) (*http.Response, error) {
				headers := make(http.Header)
				for _, value := range test.header {
					headers.Add(SessionStatusHeader, value)
				}
				return httpResponse(test.statusCode, headers, ""), nil
			})
			_, err := client.Request(context.Background(), Request{Group: "app", SessionDuration: time.Hour})
			if err == nil || !test.check(err) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestClientEnforcesResponseLimit(t *testing.T) {
	t.Run("streamed body", func(t *testing.T) {
		client := newTestClient(t, time.Second, 4, func(_ *http.Request) (*http.Response, error) {
			headers := make(http.Header)
			headers.Set(SessionStatusHeader, string(StatusNotReady))
			return httpResponse(http.StatusOK, headers, "12345"), nil
		})
		_, err := client.Request(context.Background(), Request{Group: "app", SessionDuration: time.Hour})
		if !errors.Is(err, ErrResponseTooLarge) {
			t.Fatalf("expected response limit error, got %v", err)
		}
	})

	t.Run("declared content length", func(t *testing.T) {
		body := &trackingBody{Reader: strings.NewReader("12345")}
		client := newTestClient(t, time.Second, 4, func(_ *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: body, ContentLength: 5}, nil
		})
		_, err := client.Request(context.Background(), Request{Group: "app", SessionDuration: time.Hour})
		if !errors.Is(err, ErrResponseTooLarge) || body.read {
			t.Fatalf("declared oversized response was read: err=%v read=%t", err, body.read)
		}
	})
}

func TestClientHonorsTimeoutAndCancellation(t *testing.T) {
	client := newTestClient(t, 20*time.Millisecond, 1024, func(request *http.Request) (*http.Response, error) {
		<-request.Context().Done()
		return nil, request.Context().Err()
	})
	_, err := client.Request(context.Background(), Request{Group: "app", SessionDuration: time.Hour})
	if err == nil {
		t.Fatal("expected request timeout")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.Request(ctx, Request{Group: "app", SessionDuration: time.Hour})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
}

func TestClientHandlesConcurrentRequestsIndependently(t *testing.T) {
	const requestCount = 32
	var calls atomic.Int64
	client := newTestClient(t, time.Second, 1024, func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		if request.URL.Query().Get("group") == "" {
			return nil, errors.New("missing group")
		}
		headers := make(http.Header)
		headers.Set(SessionStatusHeader, string(StatusReady))
		return httpResponse(http.StatusOK, headers, ""), nil
	})

	errorsSeen := make(chan error, requestCount)
	var requests sync.WaitGroup
	for range requestCount {
		requests.Go(func() {
			_, err := client.Request(context.Background(), Request{Group: "app", SessionDuration: time.Hour})
			errorsSeen <- err
		})
	}
	requests.Wait()
	close(errorsSeen)

	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("concurrent request failed: %v", err)
		}
	}
	if calls.Load() != requestCount {
		t.Fatalf("Sablier called %d times; want %d", calls.Load(), requestCount)
	}
}

func TestNewClientRejectsInvalidSettings(t *testing.T) {
	tests := []config.SablierConfig{
		testConfig("ftp://sablier", time.Second, 1),
		testConfig("http://user:secret@sablier", time.Second, 1),
		testConfig("http://sablier/base", time.Second, 1),
		testConfig("http://sablier?token=secret", time.Second, 1),
		testConfig("http://sablier", 0, 1),
		testConfig("http://sablier", time.Second, 0),
		testConfig("http://sablier", time.Second, config.MaxSablierResponseBytes.Value()+1),
	}
	for _, cfg := range tests {
		if _, err := NewClient(cfg); err == nil {
			t.Fatalf("expected invalid config to fail: %+v", cfg)
		}
	}
}

func TestNewClientBoundsResponseHeadersAndReusesConnections(t *testing.T) {
	client, err := NewClient(testConfig("http://sablier.test", time.Second, 1024))
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	transport, ok := client.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("unexpected transport type: %T", client.httpClient.Transport)
	}
	if transport.MaxResponseHeaderBytes != maxResponseHeaderBytes ||
		transport.MaxIdleConnsPerHost != maxIdleConnectionsPerHost {
		t.Fatalf("HTTP transport is not bounded for reuse: %+v", transport)
	}
}

func newTestClient(t *testing.T, timeout time.Duration, maxResponseBytes int64, roundTrip roundTripFunc) *Client {
	t.Helper()
	client, err := NewClient(testConfig("http://sablier.test", timeout, maxResponseBytes))
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	client.httpClient.Transport = roundTrip
	return client
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type trackingBody struct {
	io.Reader
	read bool
}

func (b *trackingBody) Read(contents []byte) (int, error) {
	b.read = true
	return b.Reader.Read(contents)
}

func (*trackingBody) Close() error {
	return nil
}

func httpResponse(statusCode int, headers http.Header, body string) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Header:     headers,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func testConfig(baseURL string, timeout time.Duration, maxResponseBytes int64) config.SablierConfig {
	return config.SablierConfig{
		BaseURL:          baseURL,
		RequestTimeout:   config.Duration(timeout),
		MaxResponseBytes: config.ByteSize(maxResponseBytes),
	}
}
