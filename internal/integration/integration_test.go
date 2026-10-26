//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/redis/go-redis/v9"
	"taskmanager/internal/cache"
	"taskmanager/internal/config"
	"taskmanager/internal/domain"
	"taskmanager/internal/httpapi"
	"taskmanager/internal/repository"
	"taskmanager/internal/service"
	"taskmanager/migrations"
)

type fixture struct {
	t     *testing.T
	db    *sql.DB
	rdb   *redis.Client
	s     *service.Service
	h     http.Handler
	users []domain.User
	teams []domain.Team
	now   time.Time
}

// Fixtures create and delete only their own rows; no shared database is truncated.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	raw := os.Getenv("TEST_MYSQL_DSN")
	if raw == "" {
		t.Fatal("TEST_MYSQL_DSN is required; use the isolated Compose test project from README")
	}
	dsn, err := mysql.ParseDSN(raw)
	if err != nil {
		t.Fatal(err)
	}
	dsn.ParseTime = true
	dsn.Loc = time.UTC
	if dsn.Params == nil {
		dsn.Params = map[string]string{}
	}
	dsn.Params["time_zone"] = "'+00:00'"
	db, err := sql.Open("mysql", dsn.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, db: db, now: time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)}
	t.Cleanup(f.cleanup)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	if err := migrations.Apply(ctx, db); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var c *cache.Cache
	if addr := os.Getenv("TEST_REDIS_ADDR"); addr != "" {
		f.rdb = redis.NewClient(&redis.Options{Addr: addr, Password: os.Getenv("TEST_REDIS_PASSWORD"), MaxRetries: -1, ContextTimeoutEnabled: true})
		if err := f.rdb.Ping(ctx).Err(); err != nil {
			t.Fatal(err)
		}
		c = cache.New(f.rdb, logger, 200*time.Millisecond)
	}
	f.s = service.New(repository.New(db), c, "integration-secret-at-least-32-bytes")
	f.s.Now = func() time.Time { return f.now }
	f.h = httpapi.New(f.s, logger, config.Config{AuthRPM: 10000, AuthBurst: 1000, APIRPM: 10000, APIBurst: 1000})
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 6; i++ {
		u, err := f.s.Register(ctx, fmt.Sprintf("it-%s-%d@example.com", hex.EncodeToString(suffix[:]), i), "password123", fmt.Sprintf("User %d", i))
		if err != nil {
			t.Fatal(err)
		}
		f.users = append(f.users, u)
	}
	for _, owner := range []int{0, 5} {
		team, err := f.s.CreateTeam(ctx, f.users[owner].ID, "Integration team")
		if err != nil {
			t.Fatal(err)
		}
		f.teams = append(f.teams, team)
	}
	for i := 1; i <= 4; i++ {
		role := domain.Member
		if i == 1 {
			role = domain.Admin
		}
		if err := f.s.Invite(ctx, f.users[0].ID, f.teams[0].ID, f.users[i].Email, role); err != nil {
			t.Fatal(err)
		}
	}
	return f
}
func (f *fixture) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if len(f.teams) > 0 {
		args := make([]any, len(f.teams))
		for i, team := range f.teams {
			args[i] = team.ID
		}
		marks := strings.TrimRight(strings.Repeat("?,", len(args)), ",")
		for _, query := range []string{
			`DELETE FROM task_comments WHERE task_id IN(SELECT id FROM tasks WHERE team_id IN(%s))`,
			`DELETE FROM task_history WHERE task_id IN(SELECT id FROM tasks WHERE team_id IN(%s))`,
			`DELETE FROM tasks WHERE team_id IN(%s)`, `DELETE FROM team_members WHERE team_id IN(%s)`, `DELETE FROM teams WHERE id IN(%s)`,
		} {
			if _, err := f.db.ExecContext(ctx, fmt.Sprintf(query, marks), args...); err != nil {
				f.t.Errorf("fixture cleanup: %v", err)
			}
		}
	}
	for _, u := range f.users {
		if _, err := f.db.ExecContext(ctx, `DELETE FROM users WHERE id=?`, u.ID); err != nil {
			f.t.Errorf("user cleanup: %v", err)
		}
	}
	if f.rdb != nil {
		_ = f.rdb.Close()
	}
	_ = f.db.Close()
}
func (f *fixture) token(user int) string {
	f.t.Helper()
	login, err := f.s.Login(context.Background(), f.users[user].Email, "password123")
	if err != nil {
		f.t.Fatal(err)
	}
	return login.AccessToken
}
func (f *fixture) request(method, path, token string, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	var b []byte
	var err error
	if body != nil {
		b, err = json.Marshal(body)
		if err != nil {
			f.t.Fatal(err)
		}
	}
	r := httptest.NewRequest(method, path, bytes.NewReader(b))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, r)
	return w
}
func requireStatus(t *testing.T, err error, status int) {
	t.Helper()
	var e *domain.Error
	if !errors.As(err, &e) || e.Status != status {
		t.Fatalf("wanted status %d, got %v", status, err)
	}
}
func (f *fixture) seed(team int, status string, assignee *int64, closed time.Time, duration time.Duration) domain.Task {
	f.t.Helper()
	created := f.now.Add(-time.Hour)
	var closedAt *time.Time
	if status == "done" {
		closedAt = &closed
		created = closed.Add(-duration)
	}
	task := domain.Task{TeamID: f.teams[team].ID, Title: "seed", Description: "", Status: status, CreatedBy: f.teams[team].CreatedBy, AssigneeID: assignee, CreatedAt: created, UpdatedAt: created, ClosedAt: closedAt, Version: 1}
	err := f.s.Store.Within(context.Background(), false, func(q *repository.Queries) error {
		if err := q.CreateTask(context.Background(), &task); err != nil {
			return err
		}
		return q.BumpRevision(context.Background(), task.TeamID)
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return task
}

func TestSQLReport(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	teamID := f.teams[0].ID
	var tasks []domain.Task
	for user, count := range map[int]int{2: 3, 1: 2, 3: 1, 4: 1} {
		for i := 0; i < count; i++ {
			closed := f.now.Add(-time.Duration(i+1) * 24 * time.Hour)
			if user == 1 && i == 1 {
				closed = f.now.Add(-30 * 24 * time.Hour)
			}
			tasks = append(tasks, f.seed(0, "done", &f.users[user].ID, closed, 2*time.Hour))
		}
	}
	tasks = append(tasks, f.seed(0, "done", &f.users[1].ID, f.now.Add(-31*24*time.Hour), time.Hour))
	tasks = append(tasks, f.seed(0, "done", nil, f.now.Add(-time.Hour), 2*time.Hour))
	tasks = append(tasks, f.seed(0, "todo", nil, time.Time{}, 0), f.seed(0, "in_progress", &f.users[2].ID, time.Time{}, 0))
	for i, n := range []int{3, 2, 1} {
		for j := 0; j < n; j++ {
			c := domain.Comment{TaskID: tasks[i].ID, UserID: f.users[0].ID, Content: "report fixture", CreatedAt: f.now}
			if err := f.s.Store.AddComment(ctx, &c); err != nil {
				t.Fatal(err)
			}
		}
	}
	foreign := f.seed(1, "done", &f.users[5].ID, f.now.Add(-time.Hour), 100*time.Hour)
	for i := 0; i < 5; i++ {
		c := domain.Comment{TaskID: foreign.ID, UserID: f.users[5].ID, Content: "foreign comment", CreatedAt: f.now}
		if err := f.s.Store.AddComment(ctx, &c); err != nil {
			t.Fatal(err)
		}
	}
	path := fmt.Sprintf("/api/v1/teams/%d/stats", teamID)
	res := f.request("GET", path, f.token(0), nil)
	if res.Code != 200 {
		t.Fatalf("%d: %s", res.Code, res.Body)
	}
	var stats domain.Stats
	if err := json.Unmarshal(res.Body.Bytes(), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.TeamID != teamID || !reflect.DeepEqual(stats.TasksByStatus, map[string]int64{"todo": 1, "in_progress": 1, "done": 9}) {
		t.Fatalf("incorrect task counts: %+v", stats)
	}
	if len(stats.TopAssignees) != 3 || stats.TopAssignees[0].UserID != f.users[2].ID || stats.TopAssignees[0].ClosedTasks != 3 || stats.TopAssignees[1].UserID != f.users[1].ID || stats.TopAssignees[1].ClosedTasks != 2 || stats.TopAssignees[2].UserID != f.users[3].ID {
		t.Fatalf("incorrect top three: %+v", stats.TopAssignees)
	}
	if stats.AverageCloseSeconds == nil || math.Abs(*stats.AverageCloseSeconds-6800) > 0.001 {
		t.Fatalf("incorrect average: %v", stats.AverageCloseSeconds)
	}
	if stats.CommentsTotal != 6 || len(stats.CommentsByTask) != 11 {
		t.Fatalf("comment aggregation incorrect: %+v", stats)
	}
	var total int64
	for _, c := range stats.CommentsByTask {
		total += c.Count
	}
	if total != 6 {
		t.Fatal("comment counts inflated by joins")
	}
	for _, tc := range []struct{ user, status int }{{1, 200}, {2, 403}, {5, 404}} {
		r := f.request("GET", path, f.token(tc.user), nil)
		if r.Code != tc.status {
			t.Fatalf("user %d: %d %s", tc.user, r.Code, r.Body)
		}
	}
	empty, err := f.s.CreateTeam(ctx, f.users[0].ID, "Empty")
	if err != nil {
		t.Fatal(err)
	}
	f.teams = append(f.teams, empty)
	zero, err := f.s.Stats(ctx, f.users[0].ID, empty.ID)
	if err != nil {
		t.Fatal(err)
	}
	if zero.CommentsTotal != 0 || zero.AverageCloseSeconds != nil || len(zero.TopAssignees) != 0 || len(zero.CommentsByTask) != 0 || !reflect.DeepEqual(zero.TasksByStatus, map[string]int64{"todo": 0, "in_progress": 0, "done": 0}) {
		t.Fatalf("incorrect empty report: %+v", zero)
	}
}

func TestConcurrencyCacheAndPermissions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	owner, member := f.users[0].ID, f.users[2].ID
	teamID := f.teams[0].ID
	task, err := f.s.CreateTask(ctx, owner, domain.TaskCreate{TeamID: teamID, Title: "Concurrent", AssigneeID: &member})
	if err != nil {
		t.Fatal(err)
	}
	filter := domain.TaskFilter{TeamID: teamID, Limit: 20, Pagination: "offset"}
	before, err := f.s.Tasks(ctx, owner, filter)
	if err != nil || len(before.Items) != 1 {
		t.Fatalf("initial list: %+v %v", before, err)
	}
	if f.rdb == nil {
		t.Fatal("TEST_REDIS_ADDR is required for cache integration checks")
	}
	team, err := f.s.Store.TeamForUser(ctx, teamID, owner, false)
	if err != nil {
		t.Fatal(err)
	}
	oldKey := domain.CacheKey(team.Revision, filter)
	if f.rdb.Exists(ctx, oldKey).Val() != 1 {
		t.Fatal("task list was not cached")
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, status := range []string{"done", "in_progress"} {
		wg.Add(1)
		go func(status string) {
			defer wg.Done()
			<-start
			_, err := f.s.UpdateTask(ctx, member, task.ID, domain.TaskUpdate{Version: 1, Status: domain.Field[string]{Set: true, Value: status}})
			results <- err
		}(status)
	}
	close(start)
	wg.Wait()
	close(results)
	success, conflicts := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else {
			requireStatus(t, err, 409)
			conflicts++
		}
	}
	if success != 1 || conflicts != 1 {
		t.Fatalf("success=%d conflicts=%d", success, conflicts)
	}
	after, err := f.s.Tasks(ctx, owner, filter)
	if err != nil || len(after.Items) != 1 || after.Items[0].Version != 2 {
		t.Fatalf("stale cache: %+v %v", after, err)
	}
	history, err := f.s.History(ctx, owner, task.ID, 100, 0)
	if err != nil || len(history) != 2 {
		t.Fatalf("history: %+v %v", history, err)
	}
	var changes map[string]domain.Change
	if err := json.Unmarshal(history[1].Changes, &changes); err != nil {
		t.Fatal(err)
	}
	if changes["status"].Old != "todo" || changes["status"].New != after.Items[0].Status || history[1].ChangedBy != member {
		t.Fatalf("history not based on actual task state: %+v", history[1])
	}
	_, err = f.s.UpdateTask(ctx, member, task.ID, domain.TaskUpdate{Version: 2, Title: domain.Field[string]{Set: true, Value: "forbidden"}})
	requireStatus(t, err, 403)
	_, err = f.s.UpdateTask(ctx, owner, task.ID, domain.TaskUpdate{Version: 2, AssigneeID: domain.Field[int64]{Set: true, Value: f.users[5].ID}})
	requireStatus(t, err, 400)
	requireStatus(t, f.s.ChangeMember(ctx, owner, teamID, member, "", true), 409)
	requireStatus(t, f.s.ChangeMember(ctx, f.users[1].ID, teamID, owner, domain.Member, false), 409)
	_, err = f.s.Tasks(ctx, f.users[5].ID, filter)
	requireStatus(t, err, 404)
	_, err = f.s.History(ctx, f.users[5].ID, task.ID, 20, 0)
	requireStatus(t, err, 404)
	_, err = f.s.AddComment(ctx, f.users[5].ID, task.ID, "not allowed")
	requireStatus(t, err, 404)
	if _, err := f.s.Tasks(ctx, f.users[3].ID, filter); err != nil {
		t.Fatal(err)
	}
	if err := f.s.ChangeMember(ctx, owner, teamID, f.users[3].ID, "", true); err != nil {
		t.Fatal(err)
	}
	_, err = f.s.Tasks(ctx, f.users[3].ID, filter)
	requireStatus(t, err, 404)
	// A loader that started before a write must keep its old snapshot and
	// revision, even if it fills Redis after the writer commits.
	err = f.s.Store.Within(ctx, true, func(q *repository.Queries) error {
		oldTeam, err := q.TeamForUser(ctx, teamID, owner, false)
		if err != nil {
			return err
		}
		updated, err := f.s.UpdateTask(ctx, owner, task.ID, domain.TaskUpdate{Version: 2, Title: domain.Field[string]{Set: true, Value: "Concurrent with cache fill"}})
		if err != nil {
			return err
		}
		items, err := q.Tasks(ctx, filter, nil)
		if err != nil {
			return err
		}
		if len(items) != 1 || items[0].Version != 2 || updated.Version != 3 {
			t.Fatal("read-only transaction did not preserve its snapshot")
		}
		f.s.Cache.Set(ctx, domain.CacheKey(oldTeam.Revision, filter), domain.TaskPage{Items: items, Limit: 20})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	refreshed, err := f.s.Tasks(ctx, owner, filter)
	if err != nil || refreshed.Items[0].Version != 3 {
		t.Fatalf("late cache fill poisoned current revision: %+v %v", refreshed, err)
	}
	for i := 0; i < 6; i++ {
		if _, err := f.s.CreateTask(ctx, owner, domain.TaskCreate{TeamID: teamID, Title: "Page " + strconv.Itoa(i)}); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[int64]bool{}
	cursorFilter := domain.TaskFilter{TeamID: teamID, Limit: 2, Pagination: "cursor"}
	for pageNumber := 0; pageNumber < 10; pageNumber++ {
		page, err := f.s.Tasks(ctx, owner, cursorFilter)
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range page.Items {
			if seen[v.ID] {
				t.Fatal("duplicate across cursor pages")
			}
			seen[v.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursorFilter.Cursor = page.NextCursor
	}
	if len(seen) != 7 {
		t.Fatalf("cursor missed tasks: %d", len(seen))
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	down := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialTimeout: 10 * time.Millisecond, ReadTimeout: 10 * time.Millisecond, WriteTimeout: 10 * time.Millisecond, ContextTimeoutEnabled: true})
	defer down.Close()
	fallback := service.New(f.s.Store, cache.New(down, logger, 20*time.Millisecond), string(f.s.Secret))
	fallback.Now = f.s.Now
	page, err := fallback.Tasks(ctx, owner, filter)
	if err != nil || len(page.Items) != 7 {
		t.Fatalf("Redis outage broke list: %+v %v", page, err)
	}
}

func TestHistoryFailureRollsBackTaskAndCacheRevision(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	owner := f.users[0].ID
	teamID := f.teams[0].ID
	task, err := f.s.CreateTask(ctx, owner, domain.TaskCreate{TeamID: teamID, Title: "Must survive rollback"})
	if err != nil {
		t.Fatal(err)
	}
	before, err := f.s.Store.TeamForUser(ctx, teamID, owner, false)
	if err != nil {
		t.Fatal(err)
	}
	// Reject only a unique fixture title. A CHECK constraint needs ordinary
	// schema privileges, unlike a trigger on a server with binary logging.
	constraint := fmt.Sprintf("it_reject_history_%d", owner)
	rejectedTitle := fmt.Sprintf("it-injected-history-failure-%d", owner)
	query := fmt.Sprintf("ALTER TABLE task_history ADD CONSTRAINT %s CHECK (JSON_UNQUOTE(JSON_EXTRACT(changes, '$.title.new')) <> '%s')", constraint, rejectedTitle)
	if _, err := f.db.ExecContext(ctx, query); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := f.db.ExecContext(context.Background(), "ALTER TABLE task_history DROP CHECK "+constraint); err != nil {
			t.Error(err)
		}
	})
	_, err = f.s.UpdateTask(ctx, owner, task.ID, domain.TaskUpdate{Version: 1, Title: domain.Field[string]{Set: true, Value: rejectedTitle}})
	if err == nil {
		t.Fatal("expected history failure")
	}
	_, err = f.s.CreateTask(ctx, owner, domain.TaskCreate{TeamID: teamID, Title: rejectedTitle})
	if err == nil {
		t.Fatal("expected creation failure")
	}
	actual, err := f.s.Store.Task(ctx, task.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if actual.Title != task.Title || actual.Version != 1 {
		t.Fatalf("task update was not rolled back: %+v", actual)
	}
	after, err := f.s.Store.TeamForUser(ctx, teamID, owner, false)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision {
		t.Fatal("cache revision committed despite rollback")
	}
	var count int
	if err := f.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE team_id=?`, teamID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("creation was not rolled back: %d tasks", count)
	}
	history, err := f.s.History(ctx, owner, task.ID, 100, 0)
	if err != nil || len(history) != 1 {
		t.Fatalf("history changed after rollback: %+v %v", history, err)
	}
}
