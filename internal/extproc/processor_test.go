package extproc

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/grpc"

	"github.com/sablierapp/sablier-extproc/internal/config"
	"github.com/sablierapp/sablier-extproc/internal/decision"
	"github.com/sablierapp/sablier-extproc/internal/observability"
	"github.com/sablierapp/sablier-extproc/internal/routing"
	"github.com/sablierapp/sablier-extproc/internal/sablier"
)

func TestRequestHeaderRoutingDecisions(t *testing.T) {
	tests := []struct {
		name          string
		authority     string
		path          string
		response      sablier.Response
		wantCalls     int
		wantImmediate bool
		wantResult    string
	}{
		{
			name: "ready mapping continues", authority: "app.example.com", path: "/private",
			response: sablier.Response{Status: sablier.StatusReady}, wantCalls: 1, wantResult: "ready",
		},
		{
			name: "unmatched continues", authority: "other.example.com", path: "/private",
			wantResult: "unmatched",
		},
		{
			name: "exclusion continues", authority: "app.example.com", path: "/healthz",
			wantResult: "excluded",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			processor, requester, metrics, logs := newTestProcessor(test.response)
			stream := &fakeProcessStream{ctx: context.Background(), requests: []*extprocv3.ProcessingRequest{
				requestHeaders(
					http.MethodGet, test.authority, test.path,
					&corev3.HeaderValue{Key: "x-request-id", RawValue: []byte("request-123")},
					&corev3.HeaderValue{Key: "authorization", RawValue: []byte("secret-token")},
					&corev3.HeaderValue{Key: "cookie", RawValue: []byte("secret-cookie")},
				),
			}}

			if err := processor.Process(stream); err != nil {
				t.Fatalf("process stream: %v", err)
			}
			if requester.calls != test.wantCalls {
				t.Fatalf("Sablier called %d times; want %d", requester.calls, test.wantCalls)
			}
			if len(stream.responses) != 1 {
				t.Fatalf("got %d responses; want 1", len(stream.responses))
			}
			isImmediate := stream.responses[0].GetImmediateResponse() != nil
			if isImmediate != test.wantImmediate {
				t.Fatalf("unexpected response: %+v", stream.responses[0])
			}
			if !strings.Contains(logs.String(), `"result":"`+test.wantResult+`"`) {
				t.Fatalf("result missing from log: %s", logs.String())
			}
			if strings.Contains(logs.String(), "secret-token") || strings.Contains(logs.String(), "secret-cookie") {
				t.Fatalf("sensitive header leaked into log: %s", logs.String())
			}
			assertMetric(t, metrics, "sablier_extproc_decisions_total", test.wantResult, 1)
		})
	}
}

func TestNotReadyGETReturnsImmediateHTML(t *testing.T) {
	processor, requester, metrics, _ := newTestProcessor(sablier.Response{
		Status:       sablier.StatusNotReady,
		Body:         []byte("<html>waiting</html>"),
		ContentType:  "text/html; charset=utf-8",
		CacheControl: "no-cache",
	})
	stream := &fakeProcessStream{ctx: context.Background(), requests: []*extprocv3.ProcessingRequest{
		requestHeaders(http.MethodGet, "app.example.com", "/private"),
	}}

	if err := processor.Process(stream); err != nil {
		t.Fatalf("process stream: %v", err)
	}
	if requester.calls != 1 || len(stream.responses) != 1 {
		t.Fatalf("unexpected calls or responses: %d, %d", requester.calls, len(stream.responses))
	}
	immediateResponse := stream.responses[0].GetImmediateResponse()
	if immediateResponse == nil || int(immediateResponse.GetStatus().GetCode()) != http.StatusOK {
		t.Fatalf("expected HTTP 200 immediate response: %+v", immediateResponse)
	}
	if string(immediateResponse.GetBody()) != "<html>waiting</html>" {
		t.Fatalf("unexpected immediate body: %q", immediateResponse.GetBody())
	}
	headers := mutationHeaders(immediateResponse.GetHeaders())
	if headers.Get("Content-Type") != "text/html; charset=utf-8" || headers.Get(sablier.SessionStatusHeader) != "not-ready" {
		t.Fatalf("unexpected immediate headers: %v", headers)
	}
	assertMetric(t, metrics, "sablier_extproc_sablier_requests_total", "not_ready", 1)
}

func TestNotReadyHEADAndPOSTProtocolResponses(t *testing.T) {
	tests := []struct {
		method        string
		statusCode    int
		body          string
		contentLength string
		retryAfter    string
	}{
		{method: http.MethodHead, statusCode: http.StatusOK, contentLength: "20"},
		{method: http.MethodPost, statusCode: http.StatusServiceUnavailable, body: "service unavailable\n", contentLength: "20", retryAfter: "5"},
	}
	for _, test := range tests {
		t.Run(test.method, func(t *testing.T) {
			processor, _, _, _ := newTestProcessor(sablier.Response{
				Status:      sablier.StatusNotReady,
				Body:        []byte("<html>waiting</html>"),
				ContentType: "text/html",
			})
			stream := &fakeProcessStream{ctx: context.Background(), requests: []*extprocv3.ProcessingRequest{
				requestHeaders(test.method, "app.example.com", "/private"),
			}}
			if err := processor.Process(stream); err != nil {
				t.Fatalf("process stream: %v", err)
			}
			response := stream.responses[0].GetImmediateResponse()
			headers := mutationHeaders(response.GetHeaders())
			if int(response.GetStatus().GetCode()) != test.statusCode || string(response.GetBody()) != test.body {
				t.Fatalf("unexpected response: %+v", response)
			}
			if headers.Get("Content-Length") != test.contentLength || headers.Get("Retry-After") != test.retryAfter {
				t.Fatalf("unexpected response headers: %v", headers)
			}
		})
	}
}

func TestInvalidAndDuplicateRequestHeaders(t *testing.T) {
	t.Run("missing pseudo header returns 400", func(t *testing.T) {
		processor, requester, _, _ := newTestProcessor(sablier.Response{Status: sablier.StatusReady})
		stream := &fakeProcessStream{ctx: context.Background(), requests: []*extprocv3.ProcessingRequest{
			requestHeaders(http.MethodGet, "", "/"),
		}}
		if err := processor.Process(stream); err != nil {
			t.Fatalf("process stream: %v", err)
		}
		response := stream.responses[0].GetImmediateResponse()
		if response == nil || int(response.GetStatus().GetCode()) != http.StatusBadRequest || requester.calls != 0 {
			t.Fatalf("unexpected invalid-request response: %+v, calls=%d", response, requester.calls)
		}
	})

	t.Run("duplicate header phase calls Sablier once", func(t *testing.T) {
		processor, requester, _, _ := newTestProcessor(sablier.Response{Status: sablier.StatusReady})
		stream := &fakeProcessStream{ctx: context.Background(), requests: []*extprocv3.ProcessingRequest{
			requestHeaders(http.MethodGet, "app.example.com", "/"),
			requestHeaders(http.MethodGet, "app.example.com", "/"),
		}}
		if err := processor.Process(stream); err != nil {
			t.Fatalf("process stream: %v", err)
		}
		if requester.calls != 1 || len(stream.responses) != 2 || stream.responses[1].GetRequestHeaders() == nil {
			t.Fatalf("duplicate phase was not a no-op: calls=%d responses=%d", requester.calls, len(stream.responses))
		}
	})
}

func TestUnexpectedPhasesReceiveMatchingNoOps(t *testing.T) {
	processor, requester, _, _ := newTestProcessor(sablier.Response{Status: sablier.StatusReady})
	stream := &fakeProcessStream{ctx: context.Background(), requests: []*extprocv3.ProcessingRequest{
		{Request: &extprocv3.ProcessingRequest_ResponseHeaders{ResponseHeaders: &extprocv3.HttpHeaders{}}},
		{Request: &extprocv3.ProcessingRequest_RequestBody{RequestBody: &extprocv3.HttpBody{}}},
		{Request: &extprocv3.ProcessingRequest_ResponseBody{ResponseBody: &extprocv3.HttpBody{}}},
		{Request: &extprocv3.ProcessingRequest_RequestTrailers{RequestTrailers: &extprocv3.HttpTrailers{}}},
		{Request: &extprocv3.ProcessingRequest_ResponseTrailers{ResponseTrailers: &extprocv3.HttpTrailers{}}},
	}}
	if err := processor.Process(stream); err != nil {
		t.Fatalf("process stream: %v", err)
	}
	if requester.calls != 0 || len(stream.responses) != 5 {
		t.Fatalf("unexpected calls or responses: %d, %d", requester.calls, len(stream.responses))
	}
	if stream.responses[0].GetResponseHeaders() == nil ||
		stream.responses[1].GetRequestBody() == nil ||
		stream.responses[2].GetResponseBody() == nil ||
		stream.responses[3].GetRequestTrailers() == nil ||
		stream.responses[4].GetResponseTrailers() == nil {
		t.Fatalf("one or more no-op responses had the wrong protocol type: %+v", stream.responses)
	}
}

func TestObservabilityModeDoesNotSendResponses(t *testing.T) {
	processor, requester, _, _ := newTestProcessor(sablier.Response{Status: sablier.StatusReady})
	request := requestHeaders(http.MethodGet, "app.example.com", "/")
	request.ObservabilityMode = true
	stream := &fakeProcessStream{ctx: context.Background(), requests: []*extprocv3.ProcessingRequest{request}}
	if err := processor.Process(stream); err != nil {
		t.Fatalf("process stream: %v", err)
	}
	if requester.calls != 0 || len(stream.responses) != 0 {
		t.Fatalf("observability message was processed: calls=%d responses=%d", requester.calls, len(stream.responses))
	}
}

func TestHeaderExtractionIgnoresSensitiveValuesAndBoundsLogs(t *testing.T) {
	sensitiveValue := strings.Repeat("secret", 100)
	values := headerValues(&extprocv3.HttpHeaders{Headers: &corev3.HeaderMap{Headers: []*corev3.HeaderValue{
		{Key: ":method", RawValue: []byte(http.MethodGet)},
		{Key: "Authorization", RawValue: []byte(sensitiveValue)},
		{Key: "Cookie", RawValue: []byte(sensitiveValue)},
	}}})
	if len(values) != 1 || values[":method"][0] != http.MethodGet {
		t.Fatalf("unexpected extracted headers: %v", values)
	}

	oversized := strings.Repeat("a", maxLoggedHeaderValueLen+1)
	if got := boundedLogValue(oversized); len(got) != maxLoggedHeaderValueLen {
		t.Fatalf("bounded log value has length %d; want %d", len(got), maxLoggedHeaderValueLen)
	}
}

func TestEnvoyStatusCodeFallsBackSafely(t *testing.T) {
	if got := envoyStatusCode(http.StatusOK); got != typev3.StatusCode_OK {
		t.Fatalf("envoyStatusCode(200) = %v", got)
	}
	if got := envoyStatusCode(999); got != typev3.StatusCode_InternalServerError {
		t.Fatalf("envoyStatusCode(999) = %v", got)
	}
}

type fakeRequester struct {
	response sablier.Response
	err      error
	calls    int
}

func (f *fakeRequester) Request(_ context.Context, _ sablier.Request) (sablier.Response, error) {
	f.calls++
	return f.response, f.err
}

type fakeProcessStream struct {
	grpc.ServerStream
	ctx       context.Context
	requests  []*extprocv3.ProcessingRequest
	responses []*extprocv3.ProcessingResponse
	position  int
}

func (f *fakeProcessStream) Context() context.Context {
	return f.ctx
}

func (f *fakeProcessStream) Recv() (*extprocv3.ProcessingRequest, error) {
	if f.position >= len(f.requests) {
		return nil, io.EOF
	}
	request := f.requests[f.position]
	f.position++
	return request, nil
}

func (f *fakeProcessStream) Send(response *extprocv3.ProcessingResponse) error {
	f.responses = append(f.responses, response)
	return nil
}

func newTestProcessor(response sablier.Response) (*Processor, *fakeRequester, *observability.Metrics, *bytes.Buffer) {
	requester := &fakeRequester{response: response}
	metrics := observability.NewMetrics("test", "test")
	logs := new(bytes.Buffer)
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	matcher := routing.New([]config.Mapping{{
		Host:       "app.example.com",
		PathPrefix: "/",
		Group:      "app",
		Exclusions: []config.Exclusion{{Type: config.ExclusionExact, Path: "/healthz"}},
	}})
	defaults := config.SablierConfig{
		SessionDuration: config.Duration(time.Hour),
		FailurePolicy:   config.FailurePolicyClosed,
		RetryAfter:      config.Duration(5 * time.Second),
	}
	return New(matcher, decision.New(requester, defaults), metrics, logger), requester, metrics, logs
}

func requestHeaders(method, authority, path string, extra ...*corev3.HeaderValue) *extprocv3.ProcessingRequest {
	headers := []*corev3.HeaderValue{
		{Key: ":method", RawValue: []byte(method)},
		{Key: ":authority", RawValue: []byte(authority)},
		{Key: ":path", RawValue: []byte(path)},
	}
	headers = append(headers, extra...)
	return &extprocv3.ProcessingRequest{Request: &extprocv3.ProcessingRequest_RequestHeaders{
		RequestHeaders: &extprocv3.HttpHeaders{Headers: &corev3.HeaderMap{Headers: headers}},
	}}
}

func mutationHeaders(mutation *extprocv3.HeaderMutation) http.Header {
	headers := make(http.Header)
	for _, option := range mutation.GetSetHeaders() {
		headers.Add(option.GetHeader().GetKey(), string(option.GetHeader().GetRawValue()))
	}
	return headers
}

func assertMetric(t *testing.T, metrics *observability.Metrics, name, result string, want float64) {
	t.Helper()
	families, err := metrics.Gatherer().Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "result" && label.GetValue() == result {
					if metric.GetCounter().GetValue() != want {
						t.Fatalf("metric %s{%s} = %f; want %f", name, result, metric.GetCounter().GetValue(), want)
					}
					return
				}
			}
		}
	}
	t.Fatalf("metric %s with result %q not found", name, result)
}
