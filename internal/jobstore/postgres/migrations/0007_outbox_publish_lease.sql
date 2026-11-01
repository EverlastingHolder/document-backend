ALTER TABLE outbox
    ADD COLUMN claim_token uuid,
    ADD COLUMN claim_until timestamptz,
    ADD CONSTRAINT outbox_claim_pair_check
        CHECK ((claim_token IS NULL) = (claim_until IS NULL)),
    ADD CONSTRAINT outbox_claim_time_check
        CHECK (claim_until IS NULL OR claim_until >= created_at);

CREATE INDEX outbox_expired_claim_idx
    ON outbox (claim_until, available_at, event_id)
    WHERE published_at IS NULL AND claim_token IS NOT NULL;
