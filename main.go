package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"x402/internal/config"
	"x402/internal/migrate"
	"x402/internal/refresh"
	"x402/internal/registry"
	"x402/internal/server"
	"x402/internal/store"
)

const (
	defaultConfigDir = "/etc/x402"
	pruneEvery       = 24 * time.Hour
)

var ready atomic.Bool

func main() {
	server.HealthcheckIfRequested("/readyz")
	if err := run(); err != nil {
		slog.Error("x402-engine stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	slog.SetDefault(logger())
	dir := os.Getenv("X402_CONFIG_DIR")
	if dir == "" {
		dir = defaultConfigDir
	}
	cfgs, err := config.Load(dir)
	if err != nil {
		return err
	}
	reg, err := registry.Load(filepath.Join(dir, "registry.yaml"))
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	st, err := store.Connect(ctx, "", "x402")
	if err != nil {
		return err
	}
	defer st.Pool.Close()
	if err := migrate.Up(ctx, st.Pool); err != nil {
		return err
	}
	m := server.NewMetrics()
	handler, err := server.New(st, reg, cfgs, m, ready.Load)
	if err != nil {
		return err
	}
	var loops sync.WaitGroup
	for _, cfg := range cfgs {
		chainReg, _ := reg.For(cfg.Chain)
		loops.Go(func() { refreshChain(ctx, st, cfg, chainReg, m) })
		if cfg.RetentionDays > 0 {
			loops.Go(func() { pruneChain(ctx, st, cfg, m) })
		}
	}
	err = server.Run(ctx, server.NewServer(server.Addr, handler), server.Debug(server.DebugAddr, m.Handler()))
	stop()
	loops.Wait()
	return err
}

func refreshChain(ctx context.Context, st *store.Store, cfg config.Config, reg *registry.Registry, m *server.Metrics) {
	refresh.Loop(ctx, st, cfg, reg, func(res refresh.Result, err error) {
		if err != nil {
			slog.Error("refresh failed", "chain", cfg.Chain, "error", err)
			return
		}
		ready.Store(true)
		if res.NotInitialised {
			slog.Info("indexer not initialised", "chain", cfg.Chain)
			return
		}
		meta, err := st.Meta(ctx, cfg.ChainID)
		if err != nil {
			slog.Error("read meta after refresh", "chain", cfg.Chain, "error", err)
			return
		}
		m.Refreshed(cfg.Chain, meta)
		level := slog.LevelDebug
		if res.Ingested > 0 || res.Reset {
			level = slog.LevelInfo
		}
		slog.Log(ctx, level, "refreshed", "chain", cfg.Chain, "ingested", res.Ingested, "dirty_days", res.DirtyDays, "reset", res.Reset,
			"watermark_block", meta.WatermarkBlock, "refresh_ms", meta.RefreshMS)
	})
}

func pruneChain(ctx context.Context, st *store.Store, cfg config.Config, m *server.Metrics) {
	refresh.PruneLoop(ctx, st, cfg, pruneEvery, func(res refresh.Pruned, err error) {
		m.Pruned(cfg.Chain, res.Payments)
		if err != nil {
			m.PruneFailed(cfg.Chain)
			slog.Error("prune failed", "chain", cfg.Chain, "engine_rows", res.Payments, "source_rows", res.Source, "error", err)
			return
		}
		slog.Info("pruned", "chain", cfg.Chain, "engine_rows", res.Payments, "source_rows", res.Source)
	})
}

func logger() *slog.Logger {
	level := slog.LevelInfo
	_ = level.UnmarshalText([]byte(os.Getenv("LOG_LEVEL")))
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}
