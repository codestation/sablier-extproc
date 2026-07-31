// Command sablier-extproc runs the Sablier Envoy external processor.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/sablierapp/sablier-extproc/internal/config"
	"github.com/sablierapp/sablier-extproc/internal/decision"
	"github.com/sablierapp/sablier-extproc/internal/extproc"
	"github.com/sablierapp/sablier-extproc/internal/observability"
	"github.com/sablierapp/sablier-extproc/internal/routing"
	"github.com/sablierapp/sablier-extproc/internal/sablier"
	"github.com/sablierapp/sablier-extproc/internal/server"
)

var (
	version  = "dev"
	revision = "unknown"
)

func main() {
	os.Exit(run())
}

func run() int {
	configFile := flag.String("config", "config.yaml", "path to the YAML configuration file")
	flag.Parse()

	bootstrapLogger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.Load(config.ResolvePath(*configFile, os.LookupEnv))
	if err != nil {
		bootstrapLogger.Error("Load configuration", slog.String("error", err.Error()))
		return 1
	}

	logger := observability.NewLogger(cfg.Logging.Level)
	sablierClient, err := sablier.NewClient(cfg.Sablier)
	if err != nil {
		logger.Error("Create Sablier client", slog.String("error", err.Error()))
		return 1
	}

	metrics := observability.NewMetrics(version, revision)
	evaluator := decision.New(sablierClient, cfg.Sablier)
	processor := extproc.New(routing.New(cfg.Mappings), evaluator, metrics, logger)
	runtime := server.New(
		cfg.Server.GRPCListenAddress,
		cfg.Server.AdminListenAddress,
		processor,
		metrics.Handler(),
		logger,
	)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := runtime.ListenAndServe(ctx); err != nil {
		logger.Error("Server stopped with error", slog.String("error", err.Error()))
		return 1
	}
	return 0
}
