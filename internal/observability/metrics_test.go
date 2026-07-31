package observability

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMetricsHandlerExposesBoundedLabelsAndBuildInfo(t *testing.T) {
	metrics := NewMetrics("1.2.3", "abc123")
	metrics.RecordDecision("app", "ready")
	metrics.RecordSablierCall("app", "ready", 25*time.Millisecond)
	metrics.StreamStarted()
	metrics.StreamFinished()

	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", http.NoBody)
	response := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("metrics status = %d", response.Code)
	}
	body := response.Body.String()
	for _, expected := range []string{
		`sablier_extproc_build_info{revision="abc123",version="1.2.3"} 1`,
		`sablier_extproc_decisions_total{group="app",result="ready"} 1`,
		`sablier_extproc_sablier_requests_total{group="app",result="ready"} 1`,
		`sablier_extproc_grpc_active_streams 0`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("metrics output does not contain %q:\n%s", expected, body)
		}
	}
	if strings.Contains(body, "host=") || strings.Contains(body, "path=") {
		t.Fatalf("unbounded host or path label exposed:\n%s", body)
	}
}
