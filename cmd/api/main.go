package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"taskmanager/internal/cache"
	"taskmanager/internal/config"
	"taskmanager/internal/httpapi"
	"taskmanager/internal/repository"
	"taskmanager/internal/service"
	"taskmanager/migrations"
)

func main() {
	if err := run(); err != nil {
		slog.Error("application stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	c, err := config.Load()
	if err != nil {
		return err
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(c.LogLevel)); err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)
	redis.SetLogger(redisLogger{logger})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db, err := sql.Open("mysql", c.DSN)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(3 * time.Minute)
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = db.PingContext(pingCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("connect to mysql: %w", err)
	}
	if len(os.Args) > 1 {
		if len(os.Args) != 2 || os.Args[1] != "migrate" {
			return fmt.Errorf("usage: api [migrate]")
		}
		return migrations.Apply(ctx, db)
	}
	rdb := redis.NewClient(&redis.Options{Addr: c.RedisAddr, Password: c.RedisPassword, DB: c.RedisDB, DialTimeout: c.RedisTimeout, ReadTimeout: c.RedisTimeout, WriteTimeout: c.RedisTimeout, ContextTimeoutEnabled: true, MaxRetries: -1})
	defer rdb.Close()
	cacheClient := cache.New(rdb, logger, c.RedisTimeout)
	svc := service.New(repository.New(db), cacheClient, c.JWTSecret)
	server := &http.Server{Addr: c.Addr, Handler: httpapi.New(svc, logger, c), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second}
	errCh := make(chan error, 1)
	go func() { logger.Info("http server started", "address", c.Addr); errCh <- server.ListenAndServe() }()
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return err
		}
		err = <-errCh
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	return nil
}

// Route go-redis's own connection-pool diagnostics through the same JSON logger.
type redisLogger struct{ logger *slog.Logger }

func (l redisLogger) Printf(ctx context.Context, format string, args ...any) {
	l.logger.WarnContext(ctx, fmt.Sprintf(format, args...), "dependency", "redis")
}
