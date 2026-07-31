// Package server runs the gRPC processor and administration HTTP servers.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthv1 "google.golang.org/grpc/health/grpc_health_v1"
)

const (
	defaultShutdownTimeout = 10 * time.Second
	adminReadTimeout       = 5 * time.Second
	adminReadHeaderTimeout = 5 * time.Second
	adminWriteTimeout      = 10 * time.Second
	adminIdleTimeout       = 30 * time.Second
	adminMaxHeaderBytes    = 16 * 1024
	maxGRPCRequestBytes    = 2 * 1024 * 1024
)

// Runtime owns the service listeners and shutdown lifecycle.
type Runtime struct {
	grpcAddress  string
	adminAddress string
	logger       *slog.Logger
	listen       func(network, address string) (net.Listener, error)

	grpcServer   *grpc.Server
	healthServer *health.Server
	adminServer  *http.Server
	ready        atomic.Bool
}

// New creates a Runtime.
func New(
	grpcAddress string,
	adminAddress string,
	processor extprocv3.ExternalProcessorServer,
	metricsHandler http.Handler,
	logger *slog.Logger,
) *Runtime {
	grpcServer := grpc.NewServer(grpc.MaxRecvMsgSize(maxGRPCRequestBytes))
	healthServer := health.NewServer()
	extprocv3.RegisterExternalProcessorServer(grpcServer, processor)
	healthv1.RegisterHealthServer(grpcServer, healthServer)

	runtime := &Runtime{
		grpcAddress:  grpcAddress,
		adminAddress: adminAddress,
		logger:       logger,
		listen:       net.Listen,
		grpcServer:   grpcServer,
		healthServer: healthServer,
	}
	runtime.adminServer = &http.Server{
		Handler:                      runtime.adminHandler(metricsHandler),
		DisableGeneralOptionsHandler: true,
		ReadTimeout:                  adminReadTimeout,
		ReadHeaderTimeout:            adminReadHeaderTimeout,
		WriteTimeout:                 adminWriteTimeout,
		IdleTimeout:                  adminIdleTimeout,
		MaxHeaderBytes:               adminMaxHeaderBytes,
		ErrorLog:                     slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	return runtime
}

// ListenAndServe opens the configured listeners and serves until shutdown.
func (r *Runtime) ListenAndServe(ctx context.Context) error {
	grpcListener, err := r.listen("tcp", r.grpcAddress)
	if err != nil {
		return fmt.Errorf("listen for gRPC: %w", err)
	}
	adminListener, err := r.listen("tcp", r.adminAddress)
	if err != nil {
		return errors.Join(fmt.Errorf("listen for admin HTTP: %w", err), closeListener(grpcListener, "gRPC"))
	}
	return r.Serve(ctx, grpcListener, adminListener)
}

// Serve runs the service on already-open listeners until shutdown.
func (r *Runtime) Serve(ctx context.Context, grpcListener, adminListener net.Listener) error {
	r.setServing(true)
	r.logger.InfoContext(
		ctx,
		"Servers started",
		slog.String("grpc_address", grpcListener.Addr().String()),
		slog.String("admin_address", adminListener.Addr().String()),
	)

	type serveResult struct {
		name string
		err  error
	}
	results := make(chan serveResult, 2)
	go func() {
		results <- serveResult{name: "grpc", err: r.grpcServer.Serve(grpcListener)}
	}()
	go func() {
		results <- serveResult{name: "admin", err: r.adminServer.Serve(adminListener)}
	}()

	var serveErr error
	select {
	case <-ctx.Done():
	case result := <-results:
		if !expectedServeError(result.err) {
			serveErr = fmt.Errorf("serve %s: %w", result.name, result.err)
		}
	}

	shutdownErr := r.shutdown(context.WithoutCancel(ctx), defaultShutdownTimeout)
	r.logger.InfoContext(ctx, "Servers stopped")
	return errors.Join(serveErr, shutdownErr)
}

// Ready reports whether the service is accepting requests.
func (r *Runtime) Ready() bool {
	return r.ready.Load()
}

func (r *Runtime) shutdown(parent context.Context, timeout time.Duration) error {
	r.setServing(false)

	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	httpErr := r.adminServer.Shutdown(ctx)

	grpcDone := make(chan struct{})
	go func() {
		r.grpcServer.GracefulStop()
		close(grpcDone)
	}()
	select {
	case <-grpcDone:
	case <-ctx.Done():
		r.grpcServer.Stop()
		<-grpcDone
	}
	if errors.Is(httpErr, http.ErrServerClosed) {
		httpErr = nil
	}
	return httpErr
}

func (r *Runtime) setServing(serving bool) {
	r.ready.Store(serving)
	status := healthv1.HealthCheckResponse_NOT_SERVING
	if serving {
		status = healthv1.HealthCheckResponse_SERVING
	}
	r.healthServer.SetServingStatus("", status)
	r.healthServer.SetServingStatus(extprocv3.ExternalProcessor_ServiceDesc.ServiceName, status)
}

func (r *Runtime) adminHandler(metricsHandler http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		writer.WriteHeader(http.StatusOK)
		if _, err := writer.Write([]byte("ok\n")); err != nil {
			r.logger.ErrorContext(request.Context(), "Write liveness response", slog.String("error", err.Error()))
		}
	})
	mux.HandleFunc("GET /readyz", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		writer.Header().Set("Cache-Control", "no-store")
		if !r.Ready() {
			writer.WriteHeader(http.StatusServiceUnavailable)
			if _, err := writer.Write([]byte("not ready\n")); err != nil {
				r.logger.ErrorContext(request.Context(), "Write readiness response", slog.String("error", err.Error()))
			}
			return
		}
		writer.WriteHeader(http.StatusOK)
		if _, err := writer.Write([]byte("ready\n")); err != nil {
			r.logger.ErrorContext(request.Context(), "Write readiness response", slog.String("error", err.Error()))
		}
	})
	mux.Handle("GET /metrics", metricsHandler)
	return mux
}

func closeListener(listener net.Listener, name string) error {
	if err := listener.Close(); err != nil {
		return fmt.Errorf("close %s listener: %w", name, err)
	}
	return nil
}

func expectedServeError(err error) bool {
	return err == nil || errors.Is(err, grpc.ErrServerStopped) || errors.Is(err, http.ErrServerClosed)
}
