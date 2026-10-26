package cache

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/sony/gobreaker/v2"
	"taskmanager/internal/domain"
)

func TestCacheTTLAndBreakerRecovery(t *testing.T) {
	r := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: r.Addr(), MaxRetries: -1, DialTimeout: 20 * time.Millisecond, ReadTimeout: 20 * time.Millisecond, WriteTimeout: 20 * time.Millisecond, ContextTimeoutEnabled: true})
	defer client.Close()
	c := newWithSettings(client, slog.New(slog.NewTextHandler(io.Discard, nil)), 50*time.Millisecond, gobreaker.Settings{MaxRequests: 1, Timeout: 20 * time.Millisecond, ReadyToTrip: func(n gobreaker.Counts) bool { return n.ConsecutiveFailures >= 5 }})
	ctx := context.Background()
	page := domain.TaskPage{Items: []domain.Task{{ID: 1}}, Limit: 20}
	if _, ok := c.Get(ctx, "missing"); ok {
		t.Fatal("unexpected hit")
	}
	if c.breaker.Counts().TotalFailures != 0 {
		t.Fatal("miss counted as failure")
	}
	c.Set(ctx, "tasks", page)
	if r.TTL("tasks") != 5*time.Minute {
		t.Fatalf("wrong TTL: %s", r.TTL("tasks"))
	}
	if p, ok := c.Get(ctx, "tasks"); !ok || len(p.Items) != 1 || p.Items[0].ID != 1 {
		t.Fatal("cache roundtrip failed")
	}
	r.FastForward(5 * time.Minute)
	if _, ok := c.Get(ctx, "tasks"); ok {
		t.Fatal("cache did not expire")
	}
	r.Close()
	for i := 0; i < 5; i++ {
		if _, ok := c.Get(ctx, "tasks"); ok {
			t.Fatal("hit during failure")
		}
	}
	if c.breaker.State() != gobreaker.StateOpen {
		t.Fatalf("breaker is %s", c.breaker.State())
	}
	before := c.breaker.Counts()
	c.Get(ctx, "tasks")
	if c.breaker.Counts() != before {
		t.Fatal("open breaker attempted request")
	}
	if err := r.Restart(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(30 * time.Millisecond)
		c.Set(ctx, "recovered", page)
		if c.breaker.State() == gobreaker.StateClosed {
			break
		}
	}
	if c.breaker.State() != gobreaker.StateClosed {
		t.Fatal("breaker failed to recover")
	}
	if _, ok := c.Get(ctx, "recovered"); !ok {
		t.Fatal("cache did not recover")
	}
}
