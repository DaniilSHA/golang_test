package cache

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/sony/gobreaker/v2"
	"taskmanager/internal/domain"
)

type Cache struct {
	client  *redis.Client
	breaker *gobreaker.CircuitBreaker[any]
	logger  *slog.Logger
	timeout time.Duration
}

func New(client *redis.Client, logger *slog.Logger, timeout time.Duration) *Cache {
	return newWithSettings(client, logger, timeout, gobreaker.Settings{
		Name: "redis", MaxRequests: 1, Interval: time.Minute, Timeout: 30 * time.Second,
		ReadyToTrip: func(c gobreaker.Counts) bool { return c.ConsecutiveFailures >= 5 },
		OnStateChange: func(name string, from, to gobreaker.State) {
			logger.Warn("circuit breaker state changed", "dependency", name, "from", from.String(), "to", to.String())
		},
	})
}
func newWithSettings(client *redis.Client, logger *slog.Logger, timeout time.Duration, settings gobreaker.Settings) *Cache {
	return &Cache{client: client, breaker: gobreaker.NewCircuitBreaker[any](settings), logger: logger, timeout: timeout}
}
func (c *Cache) execute(ctx context.Context, fn func(context.Context) (any, error)) (any, error) {
	return c.breaker.Execute(func() (any, error) {
		ctx, cancel := context.WithTimeout(ctx, c.timeout)
		defer cancel()
		return fn(ctx)
	})
}
func (c *Cache) Get(ctx context.Context, key string) (domain.TaskPage, bool) {
	var page domain.TaskPage
	if c == nil {
		return page, false
	}
	value, err := c.execute(ctx, func(ctx context.Context) (any, error) {
		b, err := c.client.Get(ctx, key).Bytes()
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		return b, err
	})
	if err != nil {
		c.logger.Debug("cache read bypassed", "error", err)
		return page, false
	}
	b, ok := value.([]byte)
	if !ok || len(b) == 0 {
		return page, false
	}
	if err := json.Unmarshal(b, &page); err != nil {
		c.logger.Warn("invalid cache payload", "error", err)
		return page, false
	}
	return page, true
}
func (c *Cache) Set(ctx context.Context, key string, page domain.TaskPage) {
	if c == nil {
		return
	}
	b, err := json.Marshal(page)
	if err != nil {
		return
	}
	_, err = c.execute(ctx, func(ctx context.Context) (any, error) { return nil, c.client.Set(ctx, key, b, 5*time.Minute).Err() })
	if err != nil {
		c.logger.Debug("cache write bypassed", "error", err)
	}
}
