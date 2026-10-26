package httpapi

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"taskmanager/docs"
	"taskmanager/internal/config"
	"taskmanager/internal/domain"
	"taskmanager/internal/service"
)

type Server struct {
	s                       *service.Service
	logger                  *slog.Logger
	authLimiter, apiLimiter *limiter
}
type endpoint func(*http.Request, int64) (any, error)

func New(s *service.Service, logger *slog.Logger, c config.Config) http.Handler {
	a := &Server{s: s, logger: logger, authLimiter: newLimiter(c.AuthRPM, c.AuthBurst), apiLimiter: newLimiter(c.APIRPM, c.APIBurst)}
	mux := http.NewServeMux()
	a.handle(mux, "POST /api/v1/register", http.StatusCreated, false, a.register)
	a.handle(mux, "POST /api/v1/login", http.StatusOK, false, a.login)
	a.handle(mux, "POST /api/v1/teams", http.StatusCreated, true, a.createTeam)
	a.handle(mux, "GET /api/v1/teams", http.StatusOK, true, func(r *http.Request, u int64) (any, error) { return s.Teams(r.Context(), u) })
	a.handle(mux, "POST /api/v1/teams/{id}/invite", http.StatusNoContent, true, a.invite)
	a.handle(mux, "PATCH /api/v1/teams/{id}/members/{user_id}", http.StatusNoContent, true, a.changeMember)
	a.handle(mux, "DELETE /api/v1/teams/{id}/members/{user_id}", http.StatusNoContent, true, a.changeMember)
	a.handle(mux, "POST /api/v1/tasks", http.StatusCreated, true, a.createTask)
	a.handle(mux, "GET /api/v1/tasks", http.StatusOK, true, a.tasks)
	a.handle(mux, "PUT /api/v1/tasks/{id}", http.StatusOK, true, a.updateTask)
	a.handle(mux, "GET /api/v1/tasks/{id}/history", http.StatusOK, true, a.history)
	a.handle(mux, "GET /api/v1/tasks/{id}/comments", http.StatusOK, true, a.comments)
	a.handle(mux, "POST /api/v1/tasks/{id}/comments", http.StatusCreated, true, a.addComment)
	a.handle(mux, "GET /api/v1/teams/{id}/stats", http.StatusOK, true, a.stats)
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Store.DB.PingContext(r.Context()); err != nil {
			writeJSON(w, 503, map[string]string{"status": "unavailable"})
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
		_, _ = w.Write(docs.OpenAPI)
	})
	mux.HandleFunc("GET /docs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(docs.Index)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { writeError(w, domain.NotFound()) })
	return a.observe(mux)
}
func (a *Server) handle(mux *http.ServeMux, pattern string, status int, protected bool, fn endpoint) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		var uid int64
		if protected {
			parts := strings.Fields(r.Header.Get("Authorization"))
			if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
				writeError(w, domain.Unauthorized())
				return
			}
			var err error
			uid, err = a.s.Authenticate(parts[1])
			if err != nil {
				writeError(w, err)
				return
			}
			if ok, retry := a.apiLimiter.allow(strconv.FormatInt(uid, 10), time.Now()); !ok {
				limited(w, retry)
				return
			}
		} else {
			ip, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				ip = r.RemoteAddr
			}
			if ok, retry := a.authLimiter.allow(ip, time.Now()); !ok {
				limited(w, retry)
				return
			}
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		result, err := fn(r, uid)
		if err != nil {
			var e *domain.Error
			if !errors.As(err, &e) {
				a.logger.Error("request failed", "request_id", w.Header().Get("X-Request-ID"), "error", err)
			}
			writeError(w, err)
			return
		}
		writeJSON(w, status, result)
	})
}
func limited(w http.ResponseWriter, retry int) {
	w.Header().Set("Retry-After", strconv.Itoa(retry))
	writeError(w, &domain.Error{Status: 429, Code: "rate_limited", Message: "request rate exceeded"})
}
func writeJSON(w http.ResponseWriter, status int, data any) {
	if status == http.StatusNoContent {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}
func writeError(w http.ResponseWriter, err error) {
	e := &domain.Error{Status: 500, Code: "internal_error", Message: "internal server error"}
	var known *domain.Error
	if errors.As(err, &known) {
		e = known
	}
	writeJSON(w, e.Status, map[string]any{"error": map[string]string{"code": e.Code, "message": e.Message}})
}
func decode(r *http.Request, out any) error {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return domain.Invalid("Content-Type must be application/json")
	}
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return domain.Invalid("invalid JSON body")
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return domain.Invalid("body must contain one JSON object")
	}
	return nil
}
func pathID(r *http.Request, key string) (int64, error) {
	v, err := strconv.ParseInt(r.PathValue(key), 10, 64)
	if err != nil || v <= 0 {
		return 0, domain.Invalid("invalid " + key)
	}
	return v, nil
}
func paging(r *http.Request) (int, int, error) {
	limit, offset := 20, 0
	var err error
	if r.URL.Query().Has("limit") {
		limit, err = strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil {
			return 0, 0, domain.Invalid("invalid limit")
		}
	}
	if r.URL.Query().Has("offset") {
		offset, err = strconv.Atoi(r.URL.Query().Get("offset"))
		if err != nil {
			return 0, 0, domain.Invalid("invalid offset")
		}
	}
	if limit < 1 || limit > 100 || offset < 0 {
		return 0, 0, domain.Invalid("limit must be 1..100 and offset non-negative")
	}
	return limit, offset, nil
}
func (a *Server) register(r *http.Request, _ int64) (any, error) {
	var b struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Name     string `json:"name"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	return a.s.Register(r.Context(), b.Email, b.Password, b.Name)
}
func (a *Server) login(r *http.Request, _ int64) (any, error) {
	var b struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	return a.s.Login(r.Context(), b.Email, b.Password)
}
func (a *Server) createTeam(r *http.Request, u int64) (any, error) {
	var b struct {
		Name string `json:"name"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	return a.s.CreateTeam(r.Context(), u, b.Name)
}
func (a *Server) invite(r *http.Request, u int64) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var b struct {
		Email string      `json:"email"`
		Role  domain.Role `json:"role"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	return nil, a.s.Invite(r.Context(), u, id, b.Email, b.Role)
}
func (a *Server) changeMember(r *http.Request, u int64) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	target, err := pathID(r, "user_id")
	if err != nil {
		return nil, err
	}
	var b struct {
		Role domain.Role `json:"role"`
	}
	remove := r.Method == http.MethodDelete
	if !remove {
		if err := decode(r, &b); err != nil {
			return nil, err
		}
	}
	return nil, a.s.ChangeMember(r.Context(), u, id, target, b.Role, remove)
}
func (a *Server) createTask(r *http.Request, u int64) (any, error) {
	var b domain.TaskCreate
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	return a.s.CreateTask(r.Context(), u, b)
}
func (a *Server) updateTask(r *http.Request, u int64) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var b domain.TaskUpdate
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	return a.s.UpdateTask(r.Context(), u, id, b)
}
func (a *Server) tasks(r *http.Request, u int64) (any, error) {
	q := r.URL.Query()
	id, err := strconv.ParseInt(q.Get("team_id"), 10, 64)
	if err != nil || id <= 0 {
		return nil, domain.Invalid("team_id is required")
	}
	limit, offset, err := paging(r)
	if err != nil {
		return nil, err
	}
	f := domain.TaskFilter{TeamID: id, Status: q.Get("status"), Limit: limit, Offset: offset, Pagination: q.Get("pagination"), Cursor: q.Get("cursor")}
	if f.Pagination == "" {
		f.Pagination = "offset"
	}
	if f.Pagination == "cursor" && q.Has("offset") {
		return nil, domain.Invalid("offset is not allowed with cursor pagination")
	}
	if q.Has("assignee_id") {
		v, err := strconv.ParseInt(q.Get("assignee_id"), 10, 64)
		if err != nil || v <= 0 {
			return nil, domain.Invalid("invalid assignee_id")
		}
		f.AssigneeID = &v
	}
	return a.s.Tasks(r.Context(), u, f)
}
func (a *Server) history(r *http.Request, u int64) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	limit, off, err := paging(r)
	if err != nil {
		return nil, err
	}
	items, err := a.s.History(r.Context(), u, id, limit, off)
	return map[string]any{"items": items, "limit": limit, "offset": off}, err
}
func (a *Server) comments(r *http.Request, u int64) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	limit, off, err := paging(r)
	if err != nil {
		return nil, err
	}
	items, err := a.s.Comments(r.Context(), u, id, limit, off)
	return map[string]any{"items": items, "limit": limit, "offset": off}, err
}
func (a *Server) addComment(r *http.Request, u int64) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	var b struct {
		Content string `json:"content"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	return a.s.AddComment(r.Context(), u, id, b.Content)
}
func (a *Server) stats(r *http.Request, u int64) (any, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return nil, err
	}
	return a.s.Stats(r.Context(), u, id)
}

type responseWriter struct {
	http.ResponseWriter
	status int
}

func (w *responseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *responseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	return w.ResponseWriter.Write(b)
}
func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (a *Server) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		var bytes [16]byte
		_, _ = rand.Read(bytes[:])
		id := hex.EncodeToString(bytes[:])
		w.Header().Set("X-Request-ID", id)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		rw := &responseWriter{ResponseWriter: w}
		defer func() {
			if recovered := recover(); recovered != nil {
				a.logger.Error("request panic", "request_id", id, "panic", recovered)
				if rw.status == 0 {
					writeError(rw, errors.New("panic"))
				}
			}
			a.logger.Info("http request", "request_id", id, "method", r.Method, "route", r.Pattern, "status", rw.status, "duration_ms", time.Since(start).Milliseconds())
		}()
		next.ServeHTTP(rw, r)
	})
}
