CREATE INDEX job_attempts_expired_lease_idx ON job_attempts (lease_expires_at) WHERE state = 'processing';
