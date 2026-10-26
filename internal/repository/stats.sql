WITH team_tasks AS (
    SELECT id, status, assignee_id, created_at, closed_at
    FROM tasks WHERE team_id = ?
), status_counts AS (
    SELECT status, COUNT(*) AS n FROM team_tasks GROUP BY status
), top_assignees AS (
    SELECT u.id AS user_id, u.name, COUNT(*) AS closed_tasks
    FROM team_tasks t JOIN users u ON u.id = t.assignee_id
    WHERE t.status = 'done'
      AND t.closed_at >= DATE_SUB(?, INTERVAL 30 DAY) AND t.closed_at <= ?
    GROUP BY u.id, u.name
    ORDER BY closed_tasks DESC, u.id ASC LIMIT 3
), comment_counts AS (
    SELECT t.id AS task_id, COUNT(c.id) AS n
    FROM team_tasks t LEFT JOIN task_comments c ON c.task_id = t.id
    GROUP BY t.id
)
SELECT
    COALESCE((SELECT JSON_OBJECTAGG(status, n) FROM status_counts), JSON_OBJECT()),
    COALESCE((SELECT JSON_ARRAYAGG(JSON_OBJECT('user_id',user_id,'name',name,'closed_tasks',closed_tasks)) FROM top_assignees), JSON_ARRAY()),
    (SELECT AVG(TIMESTAMPDIFF(MICROSECOND,created_at,closed_at)/1000000.0) FROM team_tasks WHERE status='done'),
    (SELECT COALESCE(SUM(n),0) FROM comment_counts),
    COALESCE((SELECT JSON_ARRAYAGG(JSON_OBJECT('task_id',task_id,'count',n)) FROM comment_counts), JSON_ARRAY())
