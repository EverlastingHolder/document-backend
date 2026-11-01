ALTER TABLE jobs
    ALTER COLUMN image_key DROP NOT NULL,
    ALTER COLUMN image_media_type DROP NOT NULL,
    ALTER COLUMN image_size_bytes DROP NOT NULL;

ALTER TABLE jobs
    ADD COLUMN image_sha256 bytea,
    ADD COLUMN image_purged_at timestamptz,
    ADD COLUMN image_purge_token uuid,
    ADD COLUMN image_purge_until timestamptz,
    ADD COLUMN result_purged_at timestamptz;

ALTER TABLE jobs
    ADD CONSTRAINT jobs_image_sha256_check
        CHECK ((image_sha256 IS NULL OR octet_length(image_sha256) = 32) AND (image_key IS NOT NULL OR image_sha256 IS NULL)),
    ADD CONSTRAINT jobs_image_reference_check
        CHECK (
            (image_key IS NOT NULL AND image_media_type IS NOT NULL AND image_size_bytes IS NOT NULL
                AND image_size_bytes > 0 AND image_purged_at IS NULL)
            OR
            (image_key IS NULL AND image_media_type IS NULL AND image_size_bytes IS NULL
                AND image_purged_at IS NOT NULL AND state IN ('completed','failed'))
        ),
    ADD CONSTRAINT jobs_image_purge_claim_check
        CHECK (((image_purge_token IS NULL) = (image_purge_until IS NULL)) AND
               (image_purge_token IS NULL OR (state IN ('completed','failed') AND image_key IS NOT NULL))),
    ADD CONSTRAINT jobs_result_purge_state_check
        CHECK (
            result_purged_at IS NULL
            OR (state IN ('completed', 'failed') AND result IS NULL)
        );

DO $$
DECLARE lifecycle_constraint text;
BEGIN
    SELECT conname INTO lifecycle_constraint
    FROM pg_constraint
    WHERE conrelid = 'jobs'::regclass
      AND contype = 'c'
      AND pg_get_constraintdef(oid) LIKE '%state%queued%completed%'
      AND pg_get_constraintdef(oid) LIKE '%result IS NOT NULL%';
    IF lifecycle_constraint IS NULL THEN
        RAISE EXCEPTION 'jobs lifecycle constraint not found';
    END IF;
    EXECUTE format('ALTER TABLE jobs DROP CONSTRAINT %I', lifecycle_constraint);
END $$;
ALTER TABLE jobs ADD CONSTRAINT jobs_lifecycle_data_check CHECK (
    (state = 'queued' AND finished_at IS NULL AND result IS NULL AND error_code IS NULL AND result_purged_at IS NULL)
    OR (state = 'processing' AND started_at IS NOT NULL AND finished_at IS NULL AND result IS NULL AND error_code IS NULL AND result_purged_at IS NULL)
    OR (state = 'completed' AND started_at IS NOT NULL AND finished_at IS NOT NULL AND error_code IS NULL
        AND ((result IS NOT NULL AND result_purged_at IS NULL) OR (result IS NULL AND result_purged_at IS NOT NULL)))
    OR (state = 'failed' AND finished_at IS NOT NULL AND error_code IS NOT NULL AND result IS NULL)
);

CREATE INDEX jobs_terminal_image_purge_idx
    ON jobs (finished_at, job_id)
    WHERE state IN ('completed', 'failed') AND image_key IS NOT NULL;

CREATE INDEX jobs_terminal_result_purge_idx
    ON jobs (finished_at, job_id)
    WHERE state IN ('completed', 'failed') AND result_purged_at IS NULL;
