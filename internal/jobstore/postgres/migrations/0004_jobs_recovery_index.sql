CREATE INDEX jobs_recovery_idx ON jobs (state, updated_at) WHERE state IN ('queued', 'processing');
