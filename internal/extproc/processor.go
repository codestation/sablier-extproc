// Package extproc implements Envoy's external processing service.
package extproc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	typev3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sablierapp/sablier-extproc/internal/decision"
	"github.com/sablierapp/sablier-extproc/internal/observability"
	"github.com/sablierapp/sablier-extproc/internal/routing"
)

const (
	invalidRequestBody      = "bad request\n"
	maxLoggedHeaderValueLen = 128
)

// Processor handles Envoy external processing streams.
type Processor struct {
	extprocv3.UnimplementedExternalProcessorServer

	matcher   *routing.Matcher
	evaluator *decision.Evaluator
	metrics   *observability.Metrics
	logger    *slog.Logger
}

// New creates a Processor.
func New(
	matcher *routing.Matcher,
	evaluator *decision.Evaluator,
	metrics *observability.Metrics,
	logger *slog.Logger,
) *Processor {
	return &Processor{matcher: matcher, evaluator: evaluator, metrics: metrics, logger: logger}
}

// Process handles one Envoy external processing stream.
func (p *Processor) Process(stream extprocv3.ExternalProcessor_ProcessServer) error {
	p.metrics.StreamStarted()
	defer p.metrics.StreamFinished()

	requestHeadersProcessed := false
	for {
		request, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if request.GetObservabilityMode() {
			continue
		}

		var response *extprocv3.ProcessingResponse
		terminal := false
		switch request.GetRequest().(type) {
		case *extprocv3.ProcessingRequest_RequestHeaders:
			if requestHeadersProcessed {
				response = continueRequestHeaders()
				p.metrics.RecordDecision("", "duplicate_request_headers")
				break
			}
			requestHeadersProcessed = true
			response, terminal = p.processRequestHeaders(stream.Context(), request.GetRequestHeaders())
		case *extprocv3.ProcessingRequest_ResponseHeaders:
			response = continueResponseHeaders()
			p.metrics.RecordDecision("", "unexpected_response_headers")
		case *extprocv3.ProcessingRequest_RequestBody:
			response = continueRequestBody()
			p.metrics.RecordDecision("", "unexpected_request_body")
		case *extprocv3.ProcessingRequest_ResponseBody:
			response = continueResponseBody()
			p.metrics.RecordDecision("", "unexpected_response_body")
		case *extprocv3.ProcessingRequest_RequestTrailers:
			response = continueRequestTrailers()
			p.metrics.RecordDecision("", "unexpected_request_trailers")
		case *extprocv3.ProcessingRequest_ResponseTrailers:
			response = continueResponseTrailers()
			p.metrics.RecordDecision("", "unexpected_response_trailers")
		default:
			return status.Error(codes.InvalidArgument, "processing request has no request type")
		}

		if err := stream.Send(response); err != nil {
			return err
		}
		if terminal {
			return nil
		}
	}
}

func (p *Processor) processRequestHeaders(ctx context.Context, headers *extprocv3.HttpHeaders) (*extprocv3.ProcessingResponse, bool) {
	started := time.Now()
	values := headerValues(headers)
	method, methodOK := exactlyOne(values[":method"])
	authority, authorityOK := exactlyOne(values[":authority"])
	requestTarget, pathOK := exactlyOne(values[":path"])
	requestID := first(values["x-request-id"])

	if !methodOK || !authorityOK || !pathOK {
		p.recordDecision(requestID, "", "", method, "invalid_request", started, nil)
		return immediate(typev3.StatusCode_BadRequest, http.Header{
			"Cache-Control": {"no-store"},
			"Content-Type":  {"text/plain; charset=utf-8"},
		}, []byte(invalidRequestBody), "invalid_request"), true
	}

	matched, err := p.matcher.Match(authority, requestTarget)
	if err != nil {
		p.recordDecision(requestID, "", "", method, "invalid_request", started, err)
		return immediate(typev3.StatusCode_BadRequest, http.Header{
			"Cache-Control": {"no-store"},
			"Content-Type":  {"text/plain; charset=utf-8"},
		}, []byte(invalidRequestBody), "invalid_request"), true
	}
	if matched.Mapping == nil {
		p.recordDecision(requestID, "", "", method, "unmatched", started, nil)
		return continueRequestHeaders(), false
	}

	mappingID := matched.Mapping.Host + " " + matched.Mapping.PathPrefix
	if matched.Bypass {
		p.recordDecision(requestID, mappingID, matched.Mapping.Group, method, "excluded", started, nil)
		return continueRequestHeaders(), false
	}

	sablierStarted := time.Now()
	result := p.evaluator.Evaluate(ctx, method, *matched.Mapping)
	p.metrics.RecordSablierCall(matched.Mapping.Group, sablierResult(result), time.Since(sablierStarted))
	p.recordDecision(requestID, mappingID, matched.Mapping.Group, method, string(result.Outcome), started, result.Err)
	if result.Action == decision.ActionContinue {
		return continueRequestHeaders(), false
	}
	return immediate(envoyStatusCode(result.StatusCode), result.Headers, result.Body, string(result.Outcome)), true
}

func (p *Processor) recordDecision(requestID, mapping, group, method, result string, started time.Time, err error) {
	p.metrics.RecordDecision(group, result)
	attributes := []any{
		slog.String("request_id", boundedLogValue(requestID)),
		slog.String("mapping", mapping),
		slog.String("group", group),
		slog.String("method", boundedLogValue(method)),
		slog.String("result", result),
		slog.Duration("latency", time.Since(started)),
	}
	if category := observability.ErrorCategory(err); category != "" {
		attributes = append(attributes, slog.String("error_category", category))
	}
	p.logger.Info("Request processed", attributes...)
}

func sablierResult(result decision.Result) string {
	if result.Err != nil {
		return "error"
	}
	if result.Outcome == decision.OutcomeReady {
		return "ready"
	}
	return "not_ready"
}

func headerValues(headers *extprocv3.HttpHeaders) map[string][]string {
	values := make(map[string][]string)
	if headers == nil || headers.GetHeaders() == nil {
		return values
	}
	for _, header := range headers.GetHeaders().GetHeaders() {
		if header == nil {
			continue
		}
		key := strings.ToLower(header.GetKey())
		switch key {
		case ":method", ":authority", ":path", "x-request-id":
		default:
			continue
		}
		value := header.GetValue()
		if header.RawValue != nil {
			value = string(header.GetRawValue())
		}
		values[key] = append(values[key], value)
	}
	return values
}

func boundedLogValue(value string) string {
	if len(value) <= maxLoggedHeaderValueLen {
		return value
	}
	return strings.ToValidUTF8(value[:maxLoggedHeaderValueLen], "")
}

func exactlyOne(values []string) (string, bool) {
	return first(values), len(values) == 1 && values[0] != ""
}

func first(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func immediate(statusCode typev3.StatusCode, headers http.Header, body []byte, details string) *extprocv3.ProcessingResponse {
	ownedHeaders := headers.Clone()
	if ownedHeaders.Get("Content-Length") == "" {
		ownedHeaders.Set("Content-Length", strconv.Itoa(len(body)))
	}
	keys := make([]string, 0, len(ownedHeaders))
	for key := range ownedHeaders {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	setHeaders := make([]*corev3.HeaderValueOption, 0, len(keys))
	for _, key := range keys {
		for _, value := range ownedHeaders.Values(key) {
			setHeaders = append(setHeaders, &corev3.HeaderValueOption{
				Header:       &corev3.HeaderValue{Key: key, RawValue: []byte(value)},
				AppendAction: corev3.HeaderValueOption_OVERWRITE_IF_EXISTS_OR_ADD,
			})
		}
	}

	return &extprocv3.ProcessingResponse{
		Response: &extprocv3.ProcessingResponse_ImmediateResponse{
			ImmediateResponse: &extprocv3.ImmediateResponse{
				Status:  &typev3.HttpStatus{Code: statusCode},
				Headers: &extprocv3.HeaderMutation{SetHeaders: setHeaders},
				Body:    body,
				Details: "sablier_extproc." + details,
			},
		},
	}
}

func envoyStatusCode(statusCode int) typev3.StatusCode {
	switch statusCode {
	case http.StatusOK:
		return typev3.StatusCode_OK
	case http.StatusBadRequest:
		return typev3.StatusCode_BadRequest
	case http.StatusServiceUnavailable:
		return typev3.StatusCode_ServiceUnavailable
	default:
		return typev3.StatusCode_InternalServerError
	}
}

func continueRequestHeaders() *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{Response: &extprocv3.ProcessingResponse_RequestHeaders{
		RequestHeaders: &extprocv3.HeadersResponse{Response: &extprocv3.CommonResponse{}},
	}}
}

func continueResponseHeaders() *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{Response: &extprocv3.ProcessingResponse_ResponseHeaders{
		ResponseHeaders: &extprocv3.HeadersResponse{Response: &extprocv3.CommonResponse{}},
	}}
}

func continueRequestBody() *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{Response: &extprocv3.ProcessingResponse_RequestBody{
		RequestBody: &extprocv3.BodyResponse{Response: &extprocv3.CommonResponse{}},
	}}
}

func continueResponseBody() *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{Response: &extprocv3.ProcessingResponse_ResponseBody{
		ResponseBody: &extprocv3.BodyResponse{Response: &extprocv3.CommonResponse{}},
	}}
}

func continueRequestTrailers() *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{Response: &extprocv3.ProcessingResponse_RequestTrailers{
		RequestTrailers: &extprocv3.TrailersResponse{HeaderMutation: &extprocv3.HeaderMutation{}},
	}}
}

func continueResponseTrailers() *extprocv3.ProcessingResponse {
	return &extprocv3.ProcessingResponse{Response: &extprocv3.ProcessingResponse_ResponseTrailers{
		ResponseTrailers: &extprocv3.TrailersResponse{HeaderMutation: &extprocv3.HeaderMutation{}},
	}}
}

var _ extprocv3.ExternalProcessorServer = (*Processor)(nil)
