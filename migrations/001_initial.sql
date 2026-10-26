-- +goose Up
CREATE TABLE users (
    id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    email VARCHAR(254) NOT NULL UNIQUE,
    password_hash VARCHAR(255) NOT NULL,
    name VARCHAR(200) NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6)
) ENGINE=InnoDB;

CREATE TABLE teams (
    id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    name VARCHAR(200) NOT NULL,
    created_by BIGINT NOT NULL,
    created_at DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),
    tasks_revision BIGINT NOT NULL DEFAULT 1,
    FOREIGN KEY (created_by) REFERENCES users(id)
) ENGINE=InnoDB;

CREATE TABLE team_members (
    team_id BIGINT NOT NULL,
    user_id BIGINT NOT NULL,
    role VARCHAR(16) NOT NULL,
    PRIMARY KEY (team_id, user_id),
    INDEX idx_members_user (user_id, team_id),
    FOREIGN KEY (team_id) REFERENCES teams(id),
    FOREIGN KEY (user_id) REFERENCES users(id),
    CONSTRAINT chk_role CHECK (role IN ('owner', 'admin', 'member'))
) ENGINE=InnoDB;

CREATE TABLE tasks (
    id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    team_id BIGINT NOT NULL,
    title VARCHAR(255) NOT NULL,
    description TEXT NOT NULL,
    status VARCHAR(16) NOT NULL DEFAULT 'todo',
    created_by BIGINT NOT NULL,
    assignee_id BIGINT NULL,
    created_at DATETIME(6) NOT NULL,
    updated_at DATETIME(6) NOT NULL,
    closed_at DATETIME(6) NULL,
    version BIGINT NOT NULL DEFAULT 1,
    FOREIGN KEY (team_id) REFERENCES teams(id),
    FOREIGN KEY (created_by) REFERENCES users(id),
    FOREIGN KEY (assignee_id) REFERENCES users(id),
    CONSTRAINT chk_status CHECK (status IN ('todo', 'in_progress', 'done')),
    CONSTRAINT chk_closed CHECK ((status = 'done' AND closed_at IS NOT NULL) OR (status <> 'done' AND closed_at IS NULL)),
    INDEX idx_tasks_team_created (team_id, created_at DESC, id DESC),
    INDEX idx_tasks_team_status (team_id, status, created_at DESC, id DESC),
    INDEX idx_tasks_team_assignee (team_id, assignee_id, created_at DESC, id DESC),
    INDEX idx_tasks_team_closed (team_id, status, closed_at, assignee_id)
) ENGINE=InnoDB;

CREATE TABLE task_history (
    id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    task_id BIGINT NOT NULL,
    changed_by BIGINT NOT NULL,
    changes JSON NOT NULL,
    created_at DATETIME(6) NOT NULL,
    FOREIGN KEY (task_id) REFERENCES tasks(id),
    FOREIGN KEY (changed_by) REFERENCES users(id),
    INDEX idx_history_task (task_id, id)
) ENGINE=InnoDB;

CREATE TABLE task_comments (
    id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
    task_id BIGINT NOT NULL,
    user_id BIGINT NOT NULL,
    content TEXT NOT NULL,
    created_at DATETIME(6) NOT NULL,
    FOREIGN KEY (task_id) REFERENCES tasks(id),
    FOREIGN KEY (user_id) REFERENCES users(id),
    INDEX idx_comments_task (task_id, id)
) ENGINE=InnoDB;

-- +goose Down
DROP TABLE task_comments;
DROP TABLE task_history;
DROP TABLE tasks;
DROP TABLE team_members;
DROP TABLE teams;
DROP TABLE users;
