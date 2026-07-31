package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sablierapp/sablier-extproc/internal/config"
	"github.com/sablierapp/sablier-extproc/internal/decision"
	"github.com/sablierapp/sablier-extproc/internal/extproc"
	"github.com/sablierapp/sablier-extproc/internal/observability"
	"github.com/sablierapp/sablier-extproc/internal/routing"
	"github.com/sablierapp/sablier-extproc/internal/sablier"
	"github.com/sablierapp/sablier-extproc/internal/server"
)

const envoyImage = "envoyproxy/envoy:v1.38.0@sha256:8146b97ee61a42cd216514709e4e3198af75f014974e3d9f310aef9c901fcbdf"

func TestEnvoyExternalProcessorEndToEnd(t *testing.T) {
	if os.Getenv("RUN_ENVOY_INTEGRATION") != "1" {
		t.Skip("set RUN_ENVOY_INTEGRATION=1 to run the real Envoy integration")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatalf("Docker is required: %v", err)
	}

	var sablierMode atomic.Int32
	var sablierCalls atomic.Int64
	fakeSablier := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		sablierCalls.Add(1)
		if request.URL.Path != "/api/strategies/dynamic" || request.URL.Query().Get("group") == "" {
			http.Error(writer, "bad Sablier request", http.StatusBadRequest)
			return
		}
		if sablierMode.Load() == 2 {
			http.Error(writer, "deterministic failure", http.StatusInternalServerError)
			return
		}
		status := sablier.StatusReady
		if sablierMode.Load() == 1 {
			status = sablier.StatusNotReady
		}
		writer.Header().Set(sablier.SessionStatusHeader, string(status))
		writer.Header().Set("Content-Type", "text/html; charset=utf-8")
		writer.Header().Set("Cache-Control", "no-cache")
		if _, err := writer.Write([]byte("<html>waiting</html>")); err != nil {
			t.Errorf("write fake Sablier response: %v", err)
		}
	}))
	t.Cleanup(fakeSablier.Close)

	var backendCalls atomic.Int64
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		backendCalls.Add(1)
		writer.Header().Set("Content-Type", "text/plain")
		if _, err := writer.Write([]byte("backend\n")); err != nil {
			t.Errorf("write fake backend response: %v", err)
		}
	}))
	t.Cleanup(backend.Close)

	grpcPort := freePort(t)
	adminPort := freePort(t)
	envoyPort := freePort(t)
	runtimeDone, stopRuntime := startExtProc(t, fakeSablier.URL, grpcPort, adminPort)
	t.Cleanup(func() {
		stopRuntime()
		select {
		case err := <-runtimeDone:
			if err != nil {
				t.Errorf("stop ext_proc runtime: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("ext_proc runtime did not stop")
		}
	})
	waitForHTTP(t, fmt.Sprintf("http://127.0.0.1:%d/readyz", adminPort), "")

	backendURL, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse backend URL: %v", err)
	}
	backendHost, backendPortText, err := net.SplitHostPort(backendURL.Host)
	if err != nil {
		t.Fatalf("split backend address: %v", err)
	}
	contents := envoyConfig(envoyPort, grpcPort, backendHost, backendPortText)

	containerName := fmt.Sprintf("sablier-extproc-integration-%d", time.Now().UnixNano())
	envoy := startEnvoy(t, containerName, contents)
	t.Cleanup(func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		command := exec.CommandContext(cleanupContext, "docker", "rm", "--force", containerName)
		if output, err := command.CombinedOutput(); err != nil {
			t.Errorf("remove Envoy container: %v: %s", err, output)
		}
		select {
		case <-envoy.done:
		case <-time.After(5 * time.Second):
			t.Errorf("Envoy did not stop; logs:\n%s", envoy.logs.String())
		}
	})
	baseURL := fmt.Sprintf("http://127.0.0.1:%d", envoyPort)
	waitForEnvoy(t, baseURL+"/healthz", "other.example.com", envoy)
	backendCalls.Store(0)

	sablierMode.Store(0)
	assertResponse(t, baseURL+"/app", "app.example.com", http.MethodGet, http.StatusOK, "backend\n")
	assertCounts(t, &sablierCalls, &backendCalls, 1, 1)

	sablierMode.Store(1)
	assertResponse(t, baseURL+"/app", "app.example.com", http.MethodGet, http.StatusOK, "<html>waiting</html>")
	assertCounts(t, &sablierCalls, &backendCalls, 2, 1)
	assertResponse(t, baseURL+"/app", "app.example.com", http.MethodHead, http.StatusOK, "")
	assertCounts(t, &sablierCalls, &backendCalls, 3, 1)
	assertResponse(t, baseURL+"/app", "app.example.com", http.MethodPost, http.StatusServiceUnavailable, "service unavailable\n")
	assertCounts(t, &sablierCalls, &backendCalls, 4, 1)

	sablierMode.Store(2)
	assertResponse(t, baseURL+"/app", "app.example.com", http.MethodGet, http.StatusServiceUnavailable, "service unavailable\n")
	assertCounts(t, &sablierCalls, &backendCalls, 5, 1)
	assertResponse(t, baseURL+"/app", "open.example.com", http.MethodGet, http.StatusOK, "backend\n")
	assertCounts(t, &sablierCalls, &backendCalls, 6, 2)

	assertResponse(t, baseURL+"/unmatched", "other.example.com", http.MethodGet, http.StatusOK, "backend\n")
	assertResponse(t, baseURL+"/healthz", "app.example.com", http.MethodGet, http.StatusOK, "backend\n")
	assertCounts(t, &sablierCalls, &backendCalls, 6, 4)
}

func startExtProc(t *testing.T, sablierURL string, grpcPort, adminPort int) (<-chan error, context.CancelFunc) {
	t.Helper()
	sablierConfig := config.SablierConfig{
		BaseURL:          sablierURL,
		RequestTimeout:   config.Duration(time.Second),
		MaxResponseBytes: config.ByteSize(1024 * 1024),
		SessionDuration:  config.Duration(time.Hour),
		FailurePolicy:    config.FailurePolicyClosed,
		RetryAfter:       config.Duration(time.Second),
	}
	client, err := sablier.NewClient(sablierConfig)
	if err != nil {
		t.Fatalf("create Sablier client: %v", err)
	}
	open := config.FailurePolicyOpen
	mappings := []config.Mapping{
		{
			Host:       "app.example.com",
			PathPrefix: "/",
			Group:      "app",
			Exclusions: []config.Exclusion{{Type: config.ExclusionExact, Path: "/healthz"}},
		},
		{
			Host:          "open.example.com",
			PathPrefix:    "/",
			Group:         "open-app",
			FailurePolicy: &open,
		},
	}
	metrics := observability.NewMetrics("integration", "integration")
	logger := slog.New(slog.DiscardHandler)
	processor := extproc.New(routing.New(mappings), decision.New(client, sablierConfig), metrics, logger)
	runtime := server.New(
		fmt.Sprintf("127.0.0.1:%d", grpcPort),
		fmt.Sprintf("127.0.0.1:%d", adminPort),
		processor,
		metrics.Handler(),
		logger,
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.ListenAndServe(ctx) }()
	return done, cancel
}

func startEnvoy(t *testing.T, containerName, configYAML string) *envoyProcess {
	t.Helper()
	process := &envoyProcess{logs: new(safeBuffer), done: make(chan struct{})}
	command := exec.CommandContext(
		t.Context(),
		"docker", "run", "--rm",
		"--name", containerName,
		"--network", "host",
		envoyImage,
		"--config-yaml", configYAML,
		"--log-level", "warning",
	)
	command.Stdout = process.logs
	command.Stderr = process.logs
	if err := command.Start(); err != nil {
		t.Fatalf("start Envoy: %v", err)
	}
	go func() {
		process.setError(command.Wait())
		close(process.done)
	}()
	return process
}

func assertResponse(t *testing.T, requestURL, host, method string, wantStatus int, wantBody string) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), method, requestURL, http.NoBody)
	if err != nil {
		t.Fatalf("create request: %v", err)
	}
	request.Host = host
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("request through Envoy: %v", err)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read Envoy response: %v", err)
	}
	if err := response.Body.Close(); err != nil {
		t.Fatalf("close Envoy response: %v", err)
	}
	if response.StatusCode != wantStatus || string(body) != wantBody {
		t.Fatalf("%s %s returned %d %q; want %d %q", method, requestURL, response.StatusCode, body, wantStatus, wantBody)
	}
}

func assertCounts(t *testing.T, sablierCalls, backendCalls *atomic.Int64, wantSablier, wantBackend int64) {
	t.Helper()
	if sablierCalls.Load() != wantSablier || backendCalls.Load() != wantBackend {
		t.Fatalf("calls: Sablier=%d backend=%d; want Sablier=%d backend=%d",
			sablierCalls.Load(), backendCalls.Load(), wantSablier, wantBackend)
	}
}

func waitForHTTP(t *testing.T, endpoint, host string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, endpoint, http.NoBody)
		if err != nil {
			t.Fatalf("create readiness request: %v", err)
		}
		request.Host = host
		response, err := (&http.Client{Timeout: 500 * time.Millisecond}).Do(request)
		if err == nil {
			if closeErr := response.Body.Close(); closeErr != nil {
				t.Fatalf("close readiness response: %v", closeErr)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("endpoint did not become available: %s", endpoint)
}

func waitForEnvoy(t *testing.T, endpoint, host string, process *envoyProcess) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-process.done:
			t.Fatalf("Envoy exited before becoming ready: %v\n%s", process.Error(), process.logs.String())
		default:
		}
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, endpoint, http.NoBody)
		if err != nil {
			t.Fatalf("create Envoy readiness request: %v", err)
		}
		request.Host = host
		response, err := (&http.Client{Timeout: 500 * time.Millisecond}).Do(request)
		if err == nil {
			if closeErr := response.Body.Close(); closeErr != nil {
				t.Fatalf("close Envoy readiness response: %v", closeErr)
			}
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("Envoy did not become available at %s; logs:\n%s", endpoint, process.logs.String())
}

func freePort(t *testing.T) int {
	t.Helper()
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate port: %v", err)
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("unexpected listener address type: %T", listener.Addr())
	}
	port := address.Port
	if err := listener.Close(); err != nil {
		t.Fatalf("close port listener: %v", err)
	}
	return port
}

type safeBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

type envoyProcess struct {
	logs *safeBuffer
	done chan struct{}
	mu   sync.Mutex
	err  error
}

func (p *envoyProcess) setError(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = err
}

func (p *envoyProcess) Error() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

func (b *safeBuffer) Write(contents []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(contents)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func envoyConfig(listenerPort, extProcPort int, backendHost, backendPort string) string {
	return strings.TrimSpace(fmt.Sprintf(`
static_resources:
  listeners:
    - name: ingress
      address:
        socket_address:
          address: 127.0.0.1
          port_value: %d
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: ingress
                codec_type: AUTO
                route_config:
                  name: local
                  virtual_hosts:
                    - name: all
                      domains: ["*"]
                      routes:
                        - match: { prefix: "/" }
                          route: { cluster: backend }
                http_filters:
                  - name: envoy.filters.http.ext_proc
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.ext_proc.v3.ExternalProcessor
                      grpc_service:
                        envoy_grpc:
                          cluster_name: extproc
                      failure_mode_allow: false
                      message_timeout: 5s
                      processing_mode:
                        request_header_mode: SEND
                        response_header_mode: SKIP
                        request_body_mode: NONE
                        response_body_mode: NONE
                        request_trailer_mode: SKIP
                        response_trailer_mode: SKIP
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
  clusters:
    - name: extproc
      type: STATIC
      connect_timeout: 1s
      typed_extension_protocol_options:
        envoy.extensions.upstreams.http.v3.HttpProtocolOptions:
          "@type": type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions
          explicit_http_config:
            http2_protocol_options: {}
      load_assignment:
        cluster_name: extproc
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address:
                      address: 127.0.0.1
                      port_value: %d
    - name: backend
      type: STATIC
      connect_timeout: 1s
      load_assignment:
        cluster_name: backend
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address:
                      address: %s
                      port_value: %s
`, listenerPort, extProcPort, backendHost, backendPort)) + "\n"
}
