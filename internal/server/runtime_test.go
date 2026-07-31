package server

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
)

func TestAdminHealthEndpoints(t *testing.T) {
	runtime := newTestRuntime()
	handler := runtime.adminServer.Handler

	assertHTTPStatus(t, handler, "/livez", http.StatusOK)
	assertHTTPStatus(t, handler, "/readyz", http.StatusServiceUnavailable)
	runtime.setServing(true)
	assertHTTPStatus(t, handler, "/readyz", http.StatusOK)
	runtime.setServing(false)
	assertHTTPStatus(t, handler, "/readyz", http.StatusServiceUnavailable)
}

func TestAdminServerHasResourceLimits(t *testing.T) {
	adminServer := newTestRuntime().adminServer
	if adminServer.ReadTimeout != adminReadTimeout ||
		adminServer.ReadHeaderTimeout != adminReadHeaderTimeout ||
		adminServer.WriteTimeout != adminWriteTimeout ||
		adminServer.IdleTimeout != adminIdleTimeout ||
		adminServer.MaxHeaderBytes != adminMaxHeaderBytes ||
		!adminServer.DisableGeneralOptionsHandler ||
		adminServer.ErrorLog == nil {
		t.Fatalf("admin server limits are incomplete: %+v", adminServer)
	}
}

func TestServeShutsDownGracefully(t *testing.T) {
	runtime := newTestRuntime()
	grpcListener := newBlockingListener("grpc")
	adminListener := newBlockingListener("admin")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runtime.Serve(ctx, grpcListener, adminListener)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for !runtime.Ready() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !runtime.Ready() {
		t.Fatal("runtime did not become ready")
	}
	healthResponse, err := runtime.healthServer.Check(context.Background(), &healthv1.HealthCheckRequest{
		Service: extprocv3.ExternalProcessor_ServiceDesc.ServiceName,
	})
	if err != nil || healthResponse.GetStatus() != healthv1.HealthCheckResponse_SERVING {
		t.Fatalf("gRPC health is not serving: %+v, %v", healthResponse, err)
	}

	cancel()
	select {
	case serveErr := <-done:
		if serveErr != nil {
			t.Fatalf("graceful shutdown: %v", serveErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("graceful shutdown timed out")
	}
	if runtime.Ready() {
		t.Fatal("runtime remained ready after shutdown")
	}
	healthResponse, err = runtime.healthServer.Check(context.Background(), &healthv1.HealthCheckRequest{
		Service: extprocv3.ExternalProcessor_ServiceDesc.ServiceName,
	})
	if err != nil || healthResponse.GetStatus() != healthv1.HealthCheckResponse_NOT_SERVING {
		t.Fatalf("gRPC health did not transition to not serving: %+v, %v", healthResponse, err)
	}
}

func TestListenAndServe(t *testing.T) {
	runtime := New("grpc", "admin", noopProcessor{}, http.NotFoundHandler(), slog.New(slog.DiscardHandler))
	runtime.listen = func(_ string, address string) (net.Listener, error) {
		return newBlockingListener(address), nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runtime.ListenAndServe(ctx)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for !runtime.Ready() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !runtime.Ready() {
		cancel()
		t.Fatal("runtime did not become ready")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ListenAndServe(): %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ListenAndServe did not stop")
	}
}

func TestListenAndServeReportsListenerErrors(t *testing.T) {
	t.Run("gRPC", func(t *testing.T) {
		runtime := newTestRuntime()
		runtime.listen = func(string, string) (net.Listener, error) {
			return nil, errors.New("listen failed")
		}
		if err := runtime.ListenAndServe(context.Background()); err == nil {
			t.Fatal("expected gRPC listener error")
		}
	})

	t.Run("admin", func(t *testing.T) {
		grpcListener := newBlockingListener("grpc")
		runtime := newTestRuntime()
		calls := 0
		runtime.listen = func(string, string) (net.Listener, error) {
			calls++
			if calls == 1 {
				return grpcListener, nil
			}
			return nil, errors.New("listen failed")
		}
		if err := runtime.ListenAndServe(context.Background()); err == nil {
			t.Fatal("expected admin listener error")
		}
		select {
		case <-grpcListener.closed:
		default:
			t.Fatal("gRPC listener was not closed after admin listener failure")
		}
	})
}

func TestServeReportsUnexpectedServerError(t *testing.T) {
	runtime := newTestRuntime()
	err := runtime.Serve(context.Background(), errorListener{}, newBlockingListener("admin"))
	if err == nil || !strings.Contains(err.Error(), "serve grpc") {
		t.Fatalf("expected gRPC serve error, got %v", err)
	}
}

func TestRuntimeErrorHelpers(t *testing.T) {
	closeErr := errors.New("close failed")
	if err := closeListener(errorListener{closeErr: closeErr}, "test"); !errors.Is(err, closeErr) {
		t.Fatalf("closeListener() = %v; want wrapped close error", err)
	}
	for _, err := range []error{nil, http.ErrServerClosed} {
		if !expectedServeError(err) {
			t.Fatalf("expectedServeError(%v) = false", err)
		}
	}
	if expectedServeError(errors.New("failed")) {
		t.Fatal("unexpected server error was treated as expected")
	}
}

func assertHTTPStatus(t *testing.T, handler http.Handler, path string, want int) {
	t.Helper()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, http.NoBody)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != want {
		t.Fatalf("GET %s returned %d; want %d", path, response.Code, want)
	}
}

func newTestRuntime() *Runtime {
	logger := slog.New(slog.DiscardHandler)
	return New(":18080", ":8080", noopProcessor{}, http.NotFoundHandler(), logger)
}

type noopProcessor struct {
	extprocv3.UnimplementedExternalProcessorServer
}

type blockingListener struct {
	name   string
	closed chan struct{}
	once   sync.Once
}

func newBlockingListener(name string) *blockingListener {
	return &blockingListener{name: name, closed: make(chan struct{})}
}

func (l *blockingListener) Accept() (net.Conn, error) {
	<-l.closed
	return nil, net.ErrClosed
}

func (l *blockingListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *blockingListener) Addr() net.Addr {
	return memoryAddress(l.name)
}

type memoryAddress string

func (a memoryAddress) Network() string { return "memory" }

func (a memoryAddress) String() string { return string(a) }

type errorListener struct {
	closeErr error
}

func (errorListener) Accept() (net.Conn, error) { return nil, errors.New("accept failed") }
func (l errorListener) Close() error            { return l.closeErr }
func (errorListener) Addr() net.Addr            { return memoryAddress("error") }
