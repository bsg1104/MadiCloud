-- Applications: desired state only. Nothing in this schema records what is
-- running; there is no runtime yet.
--
-- The name rules mirror application.ValidateName. They are enforced here too
-- so no writer can store a name the API would reject. COLLATE "C" makes
-- comparison and ordering byte-wise, which keeps list order and pagination
-- identical regardless of the server locale.
CREATE TABLE applications (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name          text        COLLATE "C" NOT NULL,
    desired_state text        NOT NULL DEFAULT 'active',
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT applications_name_key UNIQUE (name),
    CONSTRAINT applications_name_format CHECK (
        char_length(name) BETWEEN 3 AND 63
        AND name ~ '^[a-z][a-z0-9-]*[a-z0-9]$'
        AND strpos(name, '--') = 0
    ),
    CONSTRAINT applications_desired_state_valid CHECK (desired_state IN ('active', 'deleted')),
    CONSTRAINT applications_updated_after_created CHECK (updated_at >= created_at)
);

-- Idempotency records for retried creates. A row exists only for a request
-- that succeeded; it commits in the same transaction as the resource it
-- created. result is the snapshot returned to the original caller.
CREATE TABLE idempotency_keys (
    operation    text        NOT NULL,
    key          text        NOT NULL,
    request_hash text        NOT NULL,
    result       jsonb       NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    PRIMARY KEY (operation, key),
    CONSTRAINT idempotency_keys_key_length CHECK (char_length(key) BETWEEN 1 AND 255),
    CONSTRAINT idempotency_keys_expiry CHECK (expires_at > created_at)
);

CREATE INDEX idempotency_keys_expires_at ON idempotency_keys (expires_at);
