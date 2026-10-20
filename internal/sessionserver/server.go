// Package sessionserver hosts the runtime-neutral Sessions gRPC service.
//
// The production-shaped integration's controlplane command supplies its deployment-specific
// harness registry. The general-purpose daemon lives separately in internal/agentsessionsd.
package sessionserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"

	"google.golang.org/grpc"

	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/internal/version"
	"github.com/agent-primitives/sessions/observability"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/session"
	"github.com/agent-primitives/sessions/sqlitelog"
)

// Config contains the runtime-neutral daemon settings.
type Config struct {
	Journal          string
	Project          string
	ModelDescription string
}

// ListenAndServe opens addr and serves until ctx is cancelled or the gRPC server stops.
func ListenAndServe(ctx context.Context, addr string, cfg Config, registry *placement.Registry, logger *slog.Logger) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	return Serve(ctx, lis, cfg, registry, logger)
}

// Serve runs the Sessions service on lis. It owns the journal, listener, and gRPC server; the caller
// owns the registry and any runtime or provider resources behind that registry.
func Serve(ctx context.Context, lis net.Listener, cfg Config, registry *placement.Registry, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if cfg.Project == "" {
		cfg.Project = sqlitelog.DefaultProject
	}

	store, err := sqlitelog.Open(cfg.Journal, sqlitelog.WithDefaultProject(cfg.Project))
	if err != nil {
		_ = lis.Close()
		return fmt.Errorf("open journal %s: %w", cfg.Journal, err)
	}
	defer func() { _ = store.Close() }()

	if err := session.ReserveStaticHarnessNames(store, registry); err != nil {
		_ = lis.Close()
		return fmt.Errorf("journal: %w", err)
	}

	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(observability.UnaryServerInterceptor(logger)),
		grpc.ChainStreamInterceptor(observability.StreamServerInterceptor(logger)),
	)
	// Serve can fail while handlers are still running; drain them before closing the journal.
	defer srv.GracefulStop()
	v1.RegisterSessionsServer(srv, session.NewService(store, registry,
		session.WithLogger(logger), session.WithDefaultProject(cfg.Project)))

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(lis) }()

	logger.Info("Sessions server listening",
		"version", version.Get().Version,
		"addr", lis.Addr().String(),
		"journal", cfg.Journal,
		"project", cfg.Project,
		"harnesses", registry.Names(),
		"default_harness", registry.Default(),
		"model", cfg.ModelDescription,
	)

	select {
	case <-ctx.Done():
		logger.Info("shutting down")
		return nil
	case err := <-serveErr:
		if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			return fmt.Errorf("serve: %w", err)
		}
		return nil
	}
}
