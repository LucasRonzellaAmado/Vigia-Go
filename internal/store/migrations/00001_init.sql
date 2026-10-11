-- +goose Up
CREATE TABLE users (
    id            BIGSERIAL PRIMARY KEY,
    email         TEXT        NOT NULL UNIQUE,
    password_hash TEXT        NOT NULL,
    plan          TEXT        NOT NULL DEFAULT 'free' CHECK (plan IN ('free', 'pro')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE sessions (
    token_hash BYTEA       PRIMARY KEY,
    user_id    BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX sessions_expires_at_idx ON sessions (expires_at);

CREATE TABLE projects (
    id            BIGSERIAL PRIMARY KEY,
    user_id       BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name          TEXT        NOT NULL,
    slug          TEXT        NOT NULL UNIQUE,
    custom_domain TEXT        UNIQUE,
    public        BOOLEAN     NOT NULL DEFAULT true,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX projects_user_id_idx ON projects (user_id);

CREATE TABLE monitors (
    id                   BIGSERIAL PRIMARY KEY,
    project_id           BIGINT      NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    name                 TEXT        NOT NULL,
    kind                 TEXT        NOT NULL CHECK (kind IN ('http', 'tcp', 'ping', 'ssl', 'heartbeat')),
    target               TEXT        NOT NULL DEFAULT '',
    interval_seconds     INTEGER     NOT NULL DEFAULT 60 CHECK (interval_seconds BETWEEN 10 AND 604800),
    timeout_seconds      INTEGER     NOT NULL DEFAULT 10 CHECK (timeout_seconds BETWEEN 1 AND 86400),
    retries              INTEGER     NOT NULL DEFAULT 1 CHECK (retries BETWEEN 0 AND 5),
    failure_threshold    INTEGER     NOT NULL DEFAULT 3 CHECK (failure_threshold BETWEEN 1 AND 20),
    expected_status      INTEGER     NOT NULL DEFAULT 0,
    keyword              TEXT        NOT NULL DEFAULT '',
    ssl_expiry_days      INTEGER     NOT NULL DEFAULT 14,
    heartbeat_token      TEXT        UNIQUE,
    enabled              BOOLEAN     NOT NULL DEFAULT true,
    status               TEXT        NOT NULL DEFAULT 'unknown' CHECK (status IN ('unknown', 'up', 'down')),
    consecutive_failures INTEGER     NOT NULL DEFAULT 0,
    failing_since        TIMESTAMPTZ,
    last_checked_at      TIMESTAMPTZ,
    last_heartbeat_at    TIMESTAMPTZ,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX monitors_project_id_idx ON monitors (project_id);

CREATE TABLE check_results (
    id          BIGSERIAL PRIMARY KEY,
    monitor_id  BIGINT      NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    checked_at  TIMESTAMPTZ NOT NULL,
    ok          BOOLEAN     NOT NULL,
    latency_ms  INTEGER     NOT NULL,
    status_code INTEGER     NOT NULL DEFAULT 0,
    message     TEXT        NOT NULL DEFAULT ''
);

CREATE INDEX check_results_monitor_checked_idx ON check_results (monitor_id, checked_at DESC);
CREATE INDEX check_results_checked_at_idx ON check_results (checked_at);

CREATE TABLE incidents (
    id            BIGSERIAL PRIMARY KEY,
    monitor_id    BIGINT      NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    started_at    TIMESTAMPTZ NOT NULL,
    resolved_at   TIMESTAMPTZ,
    cause         TEXT        NOT NULL,
    last_alert_at TIMESTAMPTZ,
    alerts_sent   INTEGER     NOT NULL DEFAULT 0
);

CREATE UNIQUE INDEX incidents_one_open_per_monitor ON incidents (monitor_id) WHERE resolved_at IS NULL;
CREATE INDEX incidents_monitor_started_idx ON incidents (monitor_id, started_at DESC);

CREATE TABLE alert_channels (
    id         BIGSERIAL PRIMARY KEY,
    project_id BIGINT      NOT NULL REFERENCES projects (id) ON DELETE CASCADE,
    kind       TEXT        NOT NULL CHECK (kind IN ('telegram', 'email', 'webhook', 'whatsapp')),
    name       TEXT        NOT NULL,
    target     TEXT        NOT NULL,
    enabled    BOOLEAN     NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX alert_channels_project_id_idx ON alert_channels (project_id);

-- +goose Down
DROP TABLE alert_channels;
DROP TABLE incidents;
DROP TABLE check_results;
DROP TABLE monitors;
DROP TABLE projects;
DROP TABLE sessions;
DROP TABLE users;
