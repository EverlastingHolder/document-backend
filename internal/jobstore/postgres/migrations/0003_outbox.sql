CREATE TABLE outbox (
    event_id uuid PRIMARY KEY,
    job_id uuid NOT NULL REFERENCES jobs(job_id) ON DELETE CASCADE,
    event_type text NOT NULL CHECK (length(event_type) > 0),
    schema_version text NOT NULL CHECK (length(schema_version) > 0),
    attempt_no integer NOT NULL CHECK (attempt_no > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    available_at timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz,
    publish_attempts integer NOT NULL DEFAULT 0 CHECK (publish_attempts >= 0),
    last_error_code text CHECK (last_error_code IS NULL OR last_error_code ~ '^[a-z][a-z0-9_]{0,63}$'),
    UNIQUE (job_id, event_type, attempt_no),
    CHECK (available_at >= created_at),
    CHECK (published_at IS NULL OR published_at >= created_at)
);
