package domain

import (
	"encoding/json"
	"fmt"
	"time"
)

type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string      { return e.Message }
func Invalid(message string) error  { return &Error{400, "invalid_request", message} }
func Unauthorized() error           { return &Error{401, "unauthorized", "authentication required"} }
func Forbidden() error              { return &Error{403, "forbidden", "insufficient permissions"} }
func NotFound() error               { return &Error{404, "not_found", "resource not found"} }
func Conflict(message string) error { return &Error{409, "conflict", message} }

type Role string

const (
	Owner  Role = "owner"
	Admin  Role = "admin"
	Member Role = "member"
)

func (r Role) Manager() bool    { return r == Owner || r == Admin }
func (r Role) Assignable() bool { return r == Admin || r == Member }

type User struct {
	ID           int64     `json:"id"`
	Email        string    `json:"email"`
	Name         string    `json:"name"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
}
type Team struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	CreatedBy int64     `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	Role      Role      `json:"role"`
	Revision  int64     `json:"-"`
}
type Task struct {
	ID          int64      `json:"id"`
	TeamID      int64      `json:"team_id"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Status      string     `json:"status"`
	CreatedBy   int64      `json:"created_by"`
	AssigneeID  *int64     `json:"assignee_id"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	ClosedAt    *time.Time `json:"closed_at"`
	Version     int64      `json:"version"`
}
type Change struct {
	Old any `json:"old"`
	New any `json:"new"`
}
type History struct {
	ID        int64           `json:"id"`
	TaskID    int64           `json:"task_id"`
	ChangedBy int64           `json:"changed_by"`
	Changes   json.RawMessage `json:"changes"`
	CreatedAt time.Time       `json:"created_at"`
}
type Comment struct {
	ID        int64     `json:"id"`
	TaskID    int64     `json:"task_id"`
	UserID    int64     `json:"user_id"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}
type TaskCreate struct {
	TeamID      int64  `json:"team_id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Status      string `json:"status"`
	AssigneeID  *int64 `json:"assignee_id"`
}

// Field distinguishes a missing key from an explicit JSON null.
type Field[T any] struct {
	Set   bool
	Null  bool
	Value T
}

func (f *Field[T]) UnmarshalJSON(b []byte) error {
	f.Set = true
	if string(b) == "null" {
		f.Null = true
		return nil
	}
	return json.Unmarshal(b, &f.Value)
}

type TaskUpdate struct {
	Version     int64         `json:"version"`
	Title       Field[string] `json:"title"`
	Description Field[string] `json:"description"`
	Status      Field[string] `json:"status"`
	AssigneeID  Field[int64]  `json:"assignee_id"`
}

func ValidStatus(s string) bool { return s == "todo" || s == "in_progress" || s == "done" }
func AuthorizeUpdate(role Role, userID int64, task Task, in TaskUpdate) error {
	if role.Manager() || task.CreatedBy == userID {
		return nil
	}
	if task.AssigneeID != nil && *task.AssigneeID == userID && !in.Title.Set && !in.Description.Set && !in.AssigneeID.Set {
		return nil
	}
	return Forbidden()
}
func AuthorizeMemberChange(actor, target, requested Role) error {
	if !actor.Manager() {
		return Forbidden()
	}
	if target == Owner {
		return Conflict("the team owner cannot be removed or demoted")
	}
	if requested != "" && !requested.Assignable() {
		return Invalid("role must be admin or member")
	}
	return nil
}

type TaskFilter struct {
	TeamID     int64  `json:"team_id"`
	Status     string `json:"status"`
	AssigneeID *int64 `json:"assignee_id"`
	Limit      int    `json:"limit"`
	Offset     int    `json:"offset"`
	Pagination string `json:"pagination"`
	Cursor     string `json:"cursor"`
}
type TaskPage struct {
	Items      []Task `json:"items"`
	Limit      int    `json:"limit"`
	Offset     *int   `json:"offset,omitempty"`
	NextCursor string `json:"next_cursor,omitempty"`
}
type Cursor struct {
	TeamID     int64     `json:"team_id"`
	Status     string    `json:"status"`
	AssigneeID *int64    `json:"assignee_id"`
	CreatedAt  time.Time `json:"created_at"`
	ID         int64     `json:"id"`
}
type TopAssignee struct {
	UserID      int64  `json:"user_id"`
	Name        string `json:"name"`
	ClosedTasks int64  `json:"closed_tasks"`
}
type TaskComments struct {
	TaskID int64 `json:"task_id"`
	Count  int64 `json:"count"`
}
type Stats struct {
	TeamID              int64            `json:"team_id"`
	TasksByStatus       map[string]int64 `json:"tasks_by_status"`
	TopAssignees        []TopAssignee    `json:"top_assignees"`
	AverageCloseSeconds *float64         `json:"average_close_seconds"`
	CommentsTotal       int64            `json:"comments_total"`
	CommentsByTask      []TaskComments   `json:"comments_by_task"`
}

func CacheKey(revision int64, filter TaskFilter) string {
	b, _ := json.Marshal(filter)
	return fmt.Sprintf("tasks:v1:%d:%d:%s", filter.TeamID, revision, b)
}
