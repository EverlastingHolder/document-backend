CREATE TABLE job_attempts (
    attempt_id uuid PRIMARY KEY,
    job_id uuid NOT NULL REFERENCES jobs(job_id) ON DELETE CASCADE,
    attempt_no integer NOT NULL CHECK (attempt_no > 0),
    attempt_token uuid NOT NULL UNIQUE,
    state text NOT NULL CHECK (state IN ('processing', 'completed', 'failed')),
    started_at timestamptz NOT NULL,
    lease_expires_at timestamptz NOT NULL,
    finished_at timestamptz,
    error_code text,
    UNIQUE (job_id, attempt_no),
    CHECK (error_code IS NULL OR error_code ~ '^[a-z][a-z0-9_]{0,63}$'),
    CHECK (
        (state = 'processing' AND finished_at IS NULL AND error_code IS NULL)
        OR (state = 'completed' AND finished_at IS NOT NULL AND error_code IS NULL)
        OR (state = 'failed' AND finished_at IS NOT NULL AND error_code IS NOT NULL)
    ),
    CHECK (lease_expires_at >= started_at),
    CHECK (finished_at IS NULL OR finished_at >= started_at)
);
