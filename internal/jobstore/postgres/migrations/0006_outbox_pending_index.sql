CREATE INDEX outbox_pending_idx ON outbox (available_at, created_at, event_id) WHERE published_at IS NULL;
