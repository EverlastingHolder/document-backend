CREATE TABLE jobs (
    job_id uuid PRIMARY KEY,
    owner_id text NOT NULL CHECK (length(owner_id) > 0),
    idempotency_key text NOT NULL CHECK (idempotency_key ~ '^[!-~]{1,128}$'),
    request_digest bytea NOT NULL CHECK (octet_length(request_digest) = 32),
    image_key uuid NOT NULL UNIQUE,
    image_media_type text NOT NULL CHECK (image_media_type IN ('image/jpeg', 'image/png', 'image/webp')),
    image_size_bytes bigint NOT NULL CHECK (image_size_bytes > 0),
    pipeline_version text CHECK (pipeline_version IS NULL OR length(pipeline_version) BETWEEN 1 AND 128),
    state text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'processing', 'completed', 'failed')),
    attempt_count integer NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    available_at timestamptz NOT NULL DEFAULT now(),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    started_at timestamptz,
    finished_at timestamptz,
    result jsonb,
    error_code text,
    UNIQUE (owner_id, idempotency_key),
    CHECK (result IS NULL OR jsonb_typeof(result) = 'object'),
    CHECK (error_code IS NULL OR error_code IN ('image_unprocessable', 'processing_failed', 'ocr_unavailable', 'processing_timeout')),
    CHECK (updated_at >= created_at),
    CHECK (available_at >= created_at),
    CHECK (started_at IS NULL OR started_at >= created_at),
    CHECK (started_at IS NULL OR updated_at >= started_at),
    CHECK (finished_at IS NULL OR finished_at >= COALESCE(started_at, created_at)),
    CHECK (finished_at IS NULL OR updated_at >= finished_at),
    CHECK (
        (state = 'queued' AND finished_at IS NULL AND result IS NULL AND error_code IS NULL)
        OR (state = 'processing' AND started_at IS NOT NULL AND finished_at IS NULL AND result IS NULL AND error_code IS NULL)
        OR (state = 'completed' AND started_at IS NOT NULL AND finished_at IS NOT NULL AND result IS NOT NULL AND error_code IS NULL)
        OR (state = 'failed' AND finished_at IS NOT NULL AND result IS NULL AND error_code IS NOT NULL)
    )
);
