package repository

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"taskmanager/internal/domain"
)

type DBTX interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}
type Queries struct{ db DBTX }
type Store struct {
	DB *sql.DB
	*Queries
}

func New(db *sql.DB) *Store { return &Store{db, &Queries{db}} }

func (s *Store) Within(ctx context.Context, readOnly bool, fn func(*Queries) error) error {
	level := sql.LevelReadCommitted
	if readOnly {
		level = sql.LevelRepeatableRead
	}
	tx, err := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: level, ReadOnly: readOnly})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(&Queries{tx}); err != nil {
		return err
	}
	return tx.Commit()
}

func (q *Queries) UserByEmail(ctx context.Context, email string) (domain.User, error) {
	var u domain.User
	err := q.db.QueryRowContext(ctx, `SELECT id,email,name,password_hash,created_at FROM users WHERE email=?`, email).Scan(&u.ID, &u.Email, &u.Name, &u.PasswordHash, &u.CreatedAt)
	return u, err
}
func (q *Queries) CreateUser(ctx context.Context, u *domain.User) error {
	r, err := q.db.ExecContext(ctx, `INSERT INTO users(email,name,password_hash,created_at) VALUES(?,?,?,?)`, u.Email, u.Name, u.PasswordHash, u.CreatedAt)
	if err != nil {
		return err
	}
	u.ID, err = r.LastInsertId()
	return err
}
func (q *Queries) CreateTeam(ctx context.Context, t *domain.Team) error {
	r, err := q.db.ExecContext(ctx, `INSERT INTO teams(name,created_by,created_at) VALUES(?,?,?)`, t.Name, t.CreatedBy, t.CreatedAt)
	if err != nil {
		return err
	}
	t.ID, err = r.LastInsertId()
	return err
}
func (q *Queries) AddMember(ctx context.Context, teamID, userID int64, role domain.Role) error {
	_, err := q.db.ExecContext(ctx, `INSERT INTO team_members(team_id,user_id,role) VALUES(?,?,?)`, teamID, userID, role)
	return err
}
func (q *Queries) TeamForUser(ctx context.Context, teamID, userID int64, lock bool) (domain.Team, error) {
	if lock {
		// Lock the team before any membership row. A joined locking SELECT can
		// lock the caller's membership first and deadlock with a role change.
		var id int64
		err := q.db.QueryRowContext(ctx, `SELECT id FROM teams WHERE id=? FOR UPDATE`, teamID).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Team{}, domain.NotFound()
		}
		if err != nil {
			return domain.Team{}, err
		}
	}
	query := `SELECT t.id,t.name,t.created_by,t.created_at,t.tasks_revision,m.role FROM teams t JOIN team_members m ON m.team_id=t.id WHERE t.id=? AND m.user_id=?`
	var t domain.Team
	err := q.db.QueryRowContext(ctx, query, teamID, userID).Scan(&t.ID, &t.Name, &t.CreatedBy, &t.CreatedAt, &t.Revision, &t.Role)
	if errors.Is(err, sql.ErrNoRows) {
		err = domain.NotFound()
	}
	return t, err
}
func (q *Queries) Teams(ctx context.Context, userID int64) ([]domain.Team, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT t.id,t.name,t.created_by,t.created_at,m.role FROM teams t JOIN team_members m ON m.team_id=t.id WHERE m.user_id=? ORDER BY t.id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []domain.Team{}
	for rows.Next() {
		var t domain.Team
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedBy, &t.CreatedAt, &t.Role); err != nil {
			return nil, err
		}
		items = append(items, t)
	}
	return items, rows.Err()
}
func (q *Queries) MemberRole(ctx context.Context, teamID, userID int64) (domain.Role, error) {
	var role domain.Role
	err := q.db.QueryRowContext(ctx, `SELECT role FROM team_members WHERE team_id=? AND user_id=?`, teamID, userID).Scan(&role)
	return role, err
}
func (q *Queries) ChangeRole(ctx context.Context, teamID, userID int64, role domain.Role) error {
	_, err := q.db.ExecContext(ctx, `UPDATE team_members SET role=? WHERE team_id=? AND user_id=?`, role, teamID, userID)
	return err
}
func (q *Queries) HasAssignments(ctx context.Context, teamID, userID int64) (bool, error) {
	var found bool
	err := q.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE team_id=? AND assignee_id=?)`, teamID, userID).Scan(&found)
	return found, err
}
func (q *Queries) RemoveMember(ctx context.Context, teamID, userID int64) error {
	_, err := q.db.ExecContext(ctx, `DELETE FROM team_members WHERE team_id=? AND user_id=?`, teamID, userID)
	return err
}
func (q *Queries) BumpRevision(ctx context.Context, teamID int64) error {
	_, err := q.db.ExecContext(ctx, `UPDATE teams SET tasks_revision=tasks_revision+1 WHERE id=?`, teamID)
	return err
}

const taskColumns = `id,team_id,title,description,status,created_by,assignee_id,created_at,updated_at,closed_at,version`

type scanner interface{ Scan(...any) error }

func scanTask(row scanner) (domain.Task, error) {
	var t domain.Task
	err := row.Scan(&t.ID, &t.TeamID, &t.Title, &t.Description, &t.Status, &t.CreatedBy, &t.AssigneeID, &t.CreatedAt, &t.UpdatedAt, &t.ClosedAt, &t.Version)
	if errors.Is(err, sql.ErrNoRows) {
		err = domain.NotFound()
	}
	return t, err
}
func (q *Queries) TaskTeam(ctx context.Context, id int64) (int64, error) {
	var teamID int64
	err := q.db.QueryRowContext(ctx, `SELECT team_id FROM tasks WHERE id=?`, id).Scan(&teamID)
	if errors.Is(err, sql.ErrNoRows) {
		err = domain.NotFound()
	}
	return teamID, err
}
func (q *Queries) Task(ctx context.Context, id int64, lock bool) (domain.Task, error) {
	query := `SELECT ` + taskColumns + ` FROM tasks WHERE id=?`
	if lock {
		query += ` FOR UPDATE`
	}
	return scanTask(q.db.QueryRowContext(ctx, query, id))
}
func (q *Queries) CreateTask(ctx context.Context, t *domain.Task) error {
	r, err := q.db.ExecContext(ctx, `INSERT INTO tasks(team_id,title,description,status,created_by,assignee_id,created_at,updated_at,closed_at,version) VALUES(?,?,?,?,?,?,?,?,?,?)`, t.TeamID, t.Title, t.Description, t.Status, t.CreatedBy, t.AssigneeID, t.CreatedAt, t.UpdatedAt, t.ClosedAt, t.Version)
	if err != nil {
		return err
	}
	t.ID, err = r.LastInsertId()
	return err
}
func (q *Queries) UpdateTask(ctx context.Context, t domain.Task, expected int64) error {
	r, err := q.db.ExecContext(ctx, `UPDATE tasks SET title=?,description=?,status=?,assignee_id=?,updated_at=?,closed_at=?,version=? WHERE id=? AND version=?`, t.Title, t.Description, t.Status, t.AssigneeID, t.UpdatedAt, t.ClosedAt, t.Version, t.ID, expected)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return domain.Conflict("task version is outdated")
	}
	return nil
}
func (q *Queries) AddHistory(ctx context.Context, taskID, userID int64, changes map[string]domain.Change, now time.Time) error {
	b, err := json.Marshal(changes)
	if err != nil {
		return err
	}
	_, err = q.db.ExecContext(ctx, `INSERT INTO task_history(task_id,changed_by,changes,created_at) VALUES(?,?,?,?)`, taskID, userID, string(b), now)
	return err
}
func (q *Queries) Tasks(ctx context.Context, f domain.TaskFilter, cursor *domain.Cursor) ([]domain.Task, error) {
	query := `SELECT ` + taskColumns + ` FROM tasks WHERE team_id=?`
	args := []any{f.TeamID}
	if f.Status != "" {
		query += ` AND status=?`
		args = append(args, f.Status)
	}
	if f.AssigneeID != nil {
		query += ` AND assignee_id=?`
		args = append(args, *f.AssigneeID)
	}
	if cursor != nil {
		query += ` AND (created_at < ? OR (created_at = ? AND id < ?))`
		args = append(args, cursor.CreatedAt, cursor.CreatedAt, cursor.ID)
	}
	query += ` ORDER BY created_at DESC,id DESC LIMIT ?`
	limit := f.Limit
	if f.Pagination == "cursor" {
		limit++
	}
	args = append(args, limit)
	if f.Pagination != "cursor" {
		query += ` OFFSET ?`
		args = append(args, f.Offset)
	}
	rows, err := q.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []domain.Task{}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, t)
	}
	return items, rows.Err()
}
func (q *Queries) History(ctx context.Context, taskID int64, limit, offset int) ([]domain.History, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT id,task_id,changed_by,changes,created_at FROM task_history WHERE task_id=? ORDER BY id LIMIT ? OFFSET ?`, taskID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []domain.History{}
	for rows.Next() {
		var h domain.History
		if err := rows.Scan(&h.ID, &h.TaskID, &h.ChangedBy, &h.Changes, &h.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, h)
	}
	return items, rows.Err()
}
func (q *Queries) AddComment(ctx context.Context, c *domain.Comment) error {
	r, err := q.db.ExecContext(ctx, `INSERT INTO task_comments(task_id,user_id,content,created_at) VALUES(?,?,?,?)`, c.TaskID, c.UserID, c.Content, c.CreatedAt)
	if err != nil {
		return err
	}
	c.ID, err = r.LastInsertId()
	return err
}
func (q *Queries) Comments(ctx context.Context, taskID int64, limit, offset int) ([]domain.Comment, error) {
	rows, err := q.db.QueryContext(ctx, `SELECT id,task_id,user_id,content,created_at FROM task_comments WHERE task_id=? ORDER BY id LIMIT ? OFFSET ?`, taskID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []domain.Comment{}
	for rows.Next() {
		var c domain.Comment
		if err := rows.Scan(&c.ID, &c.TaskID, &c.UserID, &c.Content, &c.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, c)
	}
	return items, rows.Err()
}

//go:embed stats.sql
var statsSQL string

func (q *Queries) Stats(ctx context.Context, teamID int64, now time.Time) (domain.Stats, error) {
	s := domain.Stats{TeamID: teamID, TasksByStatus: map[string]int64{"todo": 0, "in_progress": 0, "done": 0}, TopAssignees: []domain.TopAssignee{}, CommentsByTask: []domain.TaskComments{}}
	var statuses, top, comments []byte
	var avg sql.NullFloat64
	err := q.db.QueryRowContext(ctx, statsSQL, teamID, now, now).Scan(&statuses, &top, &avg, &s.CommentsTotal, &comments)
	if err != nil {
		return s, err
	}
	for _, pair := range []struct {
		raw []byte
		out any
	}{{statuses, &s.TasksByStatus}, {top, &s.TopAssignees}, {comments, &s.CommentsByTask}} {
		if err := json.Unmarshal(pair.raw, pair.out); err != nil {
			return s, err
		}
	}
	if avg.Valid {
		s.AverageCloseSeconds = &avg.Float64
	}
	sort.Slice(s.TopAssignees, func(i, j int) bool {
		a, b := s.TopAssignees[i], s.TopAssignees[j]
		if a.ClosedTasks == b.ClosedTasks {
			return a.UserID < b.UserID
		}
		return a.ClosedTasks > b.ClosedTasks
	})
	sort.Slice(s.CommentsByTask, func(i, j int) bool { return s.CommentsByTask[i].TaskID < s.CommentsByTask[j].TaskID })
	return s, nil
}
