-- v002: support the outbox worker's pending/deadline predicate.
CREATE INDEX `idx_outbox_dispatch` ON `outbox_events` (`published_at`,`dead_at`,`available_at`,`id`);
