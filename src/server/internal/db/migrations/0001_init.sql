CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email         text NOT NULL UNIQUE,
    display_name  text NOT NULL DEFAULT '',
    password_hash text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE clusters (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL UNIQUE,
    owner_id   uuid NOT NULL REFERENCES users(id),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE cluster_members (
    cluster_id  uuid NOT NULL REFERENCES clusters(id) ON DELETE CASCADE,
    user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role        text NOT NULL CHECK (role IN ('owner','operator','viewer')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (cluster_id, user_id)
);

CREATE TABLE nodes (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    cluster_id    uuid NOT NULL REFERENCES clusters(id),
    name          text NOT NULL,
    machine_id    text NOT NULL,
    hostname      text NOT NULL DEFAULT '',
    os_version    text NOT NULL DEFAULT '',
    agent_version text NOT NULL DEFAULT '',
    shell_type    text NOT NULL DEFAULT '',
    public_key    text NOT NULL,
    status        text NOT NULL DEFAULT 'offline'
                  CHECK (status IN ('online','offline','disabled')),
    last_seen_at  timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (cluster_id, name),
    UNIQUE (cluster_id, machine_id)
);

CREATE TABLE enrollment_tokens (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    cluster_id  uuid NOT NULL REFERENCES clusters(id),
    token_hash  text NOT NULL UNIQUE,
    expires_at  timestamptz NOT NULL,
    max_uses    int NOT NULL DEFAULT 1,
    used_count  int NOT NULL DEFAULT 0,
    created_by  uuid NOT NULL REFERENCES users(id),
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE audit_logs (
    id         bigserial PRIMARY KEY,
    user_id    uuid REFERENCES users(id),
    cluster_id uuid,
    node_id    uuid,
    action     text NOT NULL,
    session_id text NOT NULL DEFAULT '',
    metadata   jsonb NOT NULL DEFAULT '{}',
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_nodes_status ON nodes(status);
CREATE INDEX idx_audit_created ON audit_logs(created_at);
