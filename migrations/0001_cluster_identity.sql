-- The control plane's own identity. Exactly one row, created with the schema.
-- This holds no applications, deployments, or workloads.
CREATE TABLE cluster (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    singleton  boolean     NOT NULL DEFAULT true UNIQUE CHECK (singleton),
    created_at timestamptz NOT NULL DEFAULT now()
);

INSERT INTO cluster DEFAULT VALUES;
