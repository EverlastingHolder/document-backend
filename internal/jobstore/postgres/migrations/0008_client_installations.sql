CREATE TABLE client_installations (
    client_id uuid PRIMARY KEY,
    token_sha256 bytea NOT NULL UNIQUE CHECK (octet_length(token_sha256) = 32),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    revoked_at timestamptz,
    CHECK (revoked_at IS NULL OR revoked_at >= created_at)
);
