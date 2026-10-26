package service

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/mail"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-sql-driver/mysql"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
	"taskmanager/internal/cache"
	"taskmanager/internal/domain"
	"taskmanager/internal/repository"
)

type Service struct {
	Store  *repository.Store
	Cache  *cache.Cache
	Secret []byte
	Now    func() time.Time
}

func New(store *repository.Store, c *cache.Cache, secret string) *Service {
	return &Service{Store: store, Cache: c, Secret: []byte(secret), Now: time.Now}
}
func (s *Service) now() time.Time     { return s.Now().UTC().Truncate(time.Microsecond) }
func bounded(s string, max int) bool  { return utf8.ValidString(s) && utf8.RuneCountInString(s) <= max }
func required(s string, max int) bool { return strings.TrimSpace(s) != "" && bounded(s, max) }
func emailAddress(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || len(s) > 254 {
		return "", domain.Invalid("invalid email")
	}
	return s, nil
}
func duplicate(err error) bool {
	var e *mysql.MySQLError
	return errors.As(err, &e) && e.Number == 1062
}

func (s *Service) Register(ctx context.Context, email, password, name string) (domain.User, error) {
	email, err := emailAddress(email)
	if err != nil {
		return domain.User{}, err
	}
	if len(password) < 8 || len(password) > 72 {
		return domain.User{}, domain.Invalid("password must contain 8 to 72 bytes")
	}
	if !required(name, 200) {
		return domain.User{}, domain.Invalid("name must contain 1 to 200 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return domain.User{}, err
	}
	u := domain.User{Email: email, Name: strings.TrimSpace(name), PasswordHash: string(hash), CreatedAt: s.now()}
	err = s.Store.CreateUser(ctx, &u)
	if duplicate(err) {
		err = domain.Conflict("email is already registered")
	}
	return u, err
}

type LoginResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int    `json:"expires_in"`
}

func (s *Service) Login(ctx context.Context, email, password string) (LoginResponse, error) {
	email, err := emailAddress(email)
	if err != nil {
		return LoginResponse{}, domain.Unauthorized()
	}
	u, err := s.Store.UserByEmail(ctx, email)
	if errors.Is(err, sql.ErrNoRows) {
		return LoginResponse{}, domain.Unauthorized()
	}
	if err != nil {
		return LoginResponse{}, err
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		return LoginResponse{}, domain.Unauthorized()
	}
	now := s.now()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{UserID: u.ID, RegisteredClaims: jwt.RegisteredClaims{Issuer: "taskmanager", IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour))}})
	signed, err := token.SignedString(s.Secret)
	return LoginResponse{signed, "Bearer", 3600}, err
}

type Claims struct {
	UserID int64 `json:"user_id"`
	jwt.RegisteredClaims
}

func (s *Service) Authenticate(raw string) (int64, error) {
	c := &Claims{}
	token, err := jwt.ParseWithClaims(raw, c, func(t *jwt.Token) (any, error) { return s.Secret, nil }, jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired(), jwt.WithIssuer("taskmanager"), jwt.WithTimeFunc(s.Now))
	if err != nil || !token.Valid || c.UserID <= 0 {
		return 0, domain.Unauthorized()
	}
	return c.UserID, nil
}
func (s *Service) CreateTeam(ctx context.Context, userID int64, name string) (domain.Team, error) {
	if !required(name, 200) {
		return domain.Team{}, domain.Invalid("name must contain 1 to 200 characters")
	}
	t := domain.Team{Name: strings.TrimSpace(name), CreatedBy: userID, CreatedAt: s.now(), Role: domain.Owner, Revision: 1}
	err := s.Store.Within(ctx, false, func(q *repository.Queries) error {
		if err := q.CreateTeam(ctx, &t); err != nil {
			return err
		}
		return q.AddMember(ctx, t.ID, userID, domain.Owner)
	})
	return t, err
}
func (s *Service) Teams(ctx context.Context, userID int64) ([]domain.Team, error) {
	return s.Store.Teams(ctx, userID)
}
func (s *Service) Invite(ctx context.Context, userID, teamID int64, email string, role domain.Role) error {
	if role == "" {
		role = domain.Member
	}
	if !role.Assignable() {
		return domain.Invalid("role must be admin or member")
	}
	email, err := emailAddress(email)
	if err != nil {
		return err
	}
	return s.Store.Within(ctx, false, func(q *repository.Queries) error {
		t, err := q.TeamForUser(ctx, teamID, userID, true)
		if err != nil {
			return err
		}
		if !t.Role.Manager() {
			return domain.Forbidden()
		}
		u, err := q.UserByEmail(ctx, email)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.NotFound()
		}
		if err != nil {
			return err
		}
		err = q.AddMember(ctx, teamID, u.ID, role)
		if duplicate(err) {
			return domain.Conflict("user is already a team member")
		}
		return err
	})
}
func (s *Service) ChangeMember(ctx context.Context, actorID, teamID, targetID int64, role domain.Role, remove bool) error {
	if !remove && !role.Assignable() {
		return domain.Invalid("role must be admin or member")
	}
	return s.Store.Within(ctx, false, func(q *repository.Queries) error {
		t, err := q.TeamForUser(ctx, teamID, actorID, true)
		if err != nil {
			return err
		}
		if !t.Role.Manager() {
			return domain.Forbidden()
		}
		target, err := q.MemberRole(ctx, teamID, targetID)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.NotFound()
		}
		if err != nil {
			return err
		}
		if err := domain.AuthorizeMemberChange(t.Role, target, role); err != nil {
			return err
		}
		if !remove {
			return q.ChangeRole(ctx, teamID, targetID, role)
		}
		assigned, err := q.HasAssignments(ctx, teamID, targetID)
		if err != nil {
			return err
		}
		if assigned {
			return domain.Conflict("reassign all tasks before removing this member")
		}
		return q.RemoveMember(ctx, teamID, targetID)
	})
}
func validateAssignee(ctx context.Context, q *repository.Queries, teamID int64, id *int64) error {
	if id == nil {
		return nil
	}
	if *id <= 0 {
		return domain.Invalid("invalid assignee_id")
	}
	_, err := q.MemberRole(ctx, teamID, *id)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Invalid("assignee must belong to this team")
	}
	return err
}
func (s *Service) CreateTask(ctx context.Context, userID int64, in domain.TaskCreate) (domain.Task, error) {
	if in.Status == "" {
		in.Status = "todo"
	}
	if in.TeamID <= 0 || !required(in.Title, 255) || !bounded(in.Description, 10000) || !domain.ValidStatus(in.Status) {
		return domain.Task{}, domain.Invalid("invalid team_id, title, description or status")
	}
	now := s.now()
	t := domain.Task{TeamID: in.TeamID, Title: strings.TrimSpace(in.Title), Description: in.Description, Status: in.Status, CreatedBy: userID, AssigneeID: in.AssigneeID, CreatedAt: now, UpdatedAt: now, Version: 1}
	if t.Status == "done" {
		t.ClosedAt = &now
	}
	err := s.Store.Within(ctx, false, func(q *repository.Queries) error {
		if _, err := q.TeamForUser(ctx, t.TeamID, userID, true); err != nil {
			return err
		}
		if err := validateAssignee(ctx, q, t.TeamID, t.AssigneeID); err != nil {
			return err
		}
		if err := q.CreateTask(ctx, &t); err != nil {
			return err
		}
		changes := map[string]domain.Change{"title": {New: t.Title}, "description": {New: t.Description}, "status": {New: t.Status}, "assignee_id": {New: t.AssigneeID}, "version": {New: t.Version}, "closed_at": {New: t.ClosedAt}}
		if err := q.AddHistory(ctx, t.ID, userID, changes, now); err != nil {
			return err
		}
		return q.BumpRevision(ctx, t.TeamID)
	})
	return t, err
}
func (s *Service) UpdateTask(ctx context.Context, userID, taskID int64, in domain.TaskUpdate) (domain.Task, error) {
	if in.Version <= 0 {
		return domain.Task{}, domain.Invalid("version is required")
	}
	if in.Title.Set && (in.Title.Null || !required(in.Title.Value, 255)) {
		return domain.Task{}, domain.Invalid("invalid title")
	}
	if in.Description.Set && (in.Description.Null || !bounded(in.Description.Value, 10000)) {
		return domain.Task{}, domain.Invalid("invalid description")
	}
	if in.Status.Set && (in.Status.Null || !domain.ValidStatus(in.Status.Value)) {
		return domain.Task{}, domain.Invalid("invalid status")
	}
	teamID, err := s.Store.TaskTeam(ctx, taskID)
	if err != nil {
		return domain.Task{}, err
	}
	var result domain.Task
	err = s.Store.Within(ctx, false, func(q *repository.Queries) error {
		team, err := q.TeamForUser(ctx, teamID, userID, true)
		if err != nil {
			return err
		}
		t, err := q.Task(ctx, taskID, true)
		if err != nil {
			return err
		}
		if err := domain.AuthorizeUpdate(team.Role, userID, t, in); err != nil {
			return err
		}
		if t.Version != in.Version {
			return domain.Conflict("task version is outdated")
		}
		old := t
		if in.Title.Set {
			t.Title = strings.TrimSpace(in.Title.Value)
		}
		if in.Description.Set {
			t.Description = in.Description.Value
		}
		if in.Status.Set {
			t.Status = in.Status.Value
		}
		if in.AssigneeID.Set {
			t.AssigneeID = nil
			if !in.AssigneeID.Null {
				v := in.AssigneeID.Value
				t.AssigneeID = &v
			}
			if err := validateAssignee(ctx, q, teamID, t.AssigneeID); err != nil {
				return err
			}
		}
		now := s.now()
		if old.Status != t.Status {
			if t.Status == "done" {
				t.ClosedAt = &now
			} else {
				t.ClosedAt = nil
			}
		}
		changes := map[string]domain.Change{}
		for _, f := range []struct {
			name     string
			old, new any
		}{{"title", old.Title, t.Title}, {"description", old.Description, t.Description}, {"status", old.Status, t.Status}, {"assignee_id", old.AssigneeID, t.AssigneeID}, {"closed_at", old.ClosedAt, t.ClosedAt}} {
			if !reflect.DeepEqual(f.old, f.new) {
				changes[f.name] = domain.Change{Old: f.old, New: f.new}
			}
		}
		if len(changes) == 0 {
			result = t
			return nil
		}
		t.Version++
		t.UpdatedAt = now
		changes["version"] = domain.Change{Old: old.Version, New: t.Version}
		if err := q.UpdateTask(ctx, t, in.Version); err != nil {
			return err
		}
		if err := q.AddHistory(ctx, t.ID, userID, changes, now); err != nil {
			return err
		}
		if err := q.BumpRevision(ctx, teamID); err != nil {
			return err
		}
		result = t
		return nil
	})
	return result, err
}
func decodeCursor(f domain.TaskFilter) (*domain.Cursor, error) {
	if f.Cursor == "" {
		return nil, nil
	}
	if len(f.Cursor) > 4096 {
		return nil, domain.Invalid("invalid cursor")
	}
	b, err := base64.RawURLEncoding.DecodeString(f.Cursor)
	if err != nil {
		return nil, domain.Invalid("invalid cursor")
	}
	var c domain.Cursor
	if json.Unmarshal(b, &c) != nil || c.ID <= 0 || c.CreatedAt.IsZero() || c.TeamID != f.TeamID || c.Status != f.Status || !reflect.DeepEqual(c.AssigneeID, f.AssigneeID) {
		return nil, domain.Invalid("cursor does not match query")
	}
	return &c, nil
}
func (s *Service) Tasks(ctx context.Context, userID int64, f domain.TaskFilter) (domain.TaskPage, error) {
	if f.TeamID <= 0 || f.Limit < 1 || f.Limit > 100 || f.Offset < 0 || (f.Status != "" && !domain.ValidStatus(f.Status)) || (f.AssigneeID != nil && *f.AssigneeID <= 0) {
		return domain.TaskPage{}, domain.Invalid("invalid task filters")
	}
	if f.Pagination != "offset" && f.Pagination != "cursor" {
		return domain.TaskPage{}, domain.Invalid("pagination must be offset or cursor")
	}
	if (f.Pagination == "cursor" && f.Offset != 0) || (f.Pagination == "offset" && f.Cursor != "") {
		return domain.TaskPage{}, domain.Invalid("cursor and offset pagination cannot be combined")
	}
	cursor, err := decodeCursor(f)
	if err != nil {
		return domain.TaskPage{}, err
	}
	var page domain.TaskPage
	var key string
	hit := false
	err = s.Store.Within(ctx, true, func(q *repository.Queries) error {
		t, err := q.TeamForUser(ctx, f.TeamID, userID, false)
		if err != nil {
			return err
		}
		key = domain.CacheKey(t.Revision, f)
		if cached, ok := s.Cache.Get(ctx, key); ok {
			page = cached
			hit = true
			return nil
		}
		items, err := q.Tasks(ctx, f, cursor)
		if err != nil {
			return err
		}
		page = domain.TaskPage{Items: items, Limit: f.Limit}
		if f.Pagination == "offset" {
			off := f.Offset
			page.Offset = &off
		} else if len(items) > f.Limit {
			page.Items = items[:f.Limit]
			last := page.Items[len(page.Items)-1]
			b, _ := json.Marshal(domain.Cursor{TeamID: f.TeamID, Status: f.Status, AssigneeID: f.AssigneeID, CreatedAt: last.CreatedAt, ID: last.ID})
			page.NextCursor = base64.RawURLEncoding.EncodeToString(b)
		}
		return nil
	})
	if err == nil && !hit {
		s.Cache.Set(ctx, key, page)
	}
	return page, err
}
func (s *Service) History(ctx context.Context, userID, taskID int64, limit, offset int) ([]domain.History, error) {
	var items []domain.History
	err := s.withTaskAccess(ctx, userID, taskID, true, func(q *repository.Queries) error {
		var err error
		items, err = q.History(ctx, taskID, limit, offset)
		return err
	})
	return items, err
}
func (s *Service) Comments(ctx context.Context, userID, taskID int64, limit, offset int) ([]domain.Comment, error) {
	var items []domain.Comment
	err := s.withTaskAccess(ctx, userID, taskID, true, func(q *repository.Queries) error {
		var err error
		items, err = q.Comments(ctx, taskID, limit, offset)
		return err
	})
	return items, err
}
func (s *Service) AddComment(ctx context.Context, userID, taskID int64, content string) (domain.Comment, error) {
	if !required(content, 10000) {
		return domain.Comment{}, domain.Invalid("content must contain 1 to 10000 characters")
	}
	c := domain.Comment{TaskID: taskID, UserID: userID, Content: content, CreatedAt: s.now()}
	err := s.withTaskAccess(ctx, userID, taskID, false, func(q *repository.Queries) error { return q.AddComment(ctx, &c) })
	return c, err
}
func (s *Service) withTaskAccess(ctx context.Context, userID, taskID int64, readOnly bool, fn func(*repository.Queries) error) error {
	teamID, err := s.Store.TaskTeam(ctx, taskID)
	if err != nil {
		return err
	}
	return s.Store.Within(ctx, readOnly, func(q *repository.Queries) error {
		if _, err := q.TeamForUser(ctx, teamID, userID, !readOnly); err != nil {
			return err
		}
		return fn(q)
	})
}
func (s *Service) Stats(ctx context.Context, userID, teamID int64) (domain.Stats, error) {
	var stats domain.Stats
	err := s.Store.Within(ctx, true, func(q *repository.Queries) error {
		t, err := q.TeamForUser(ctx, teamID, userID, false)
		if err != nil {
			return err
		}
		if !t.Role.Manager() {
			return domain.Forbidden()
		}
		stats, err = q.Stats(ctx, teamID, s.now())
		return err
	})
	return stats, err
}
