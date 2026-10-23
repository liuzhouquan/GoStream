-- v002: support the outbox worker's pending/deadline predicate.
CREATE INDEX IF NOT EXISTS `idx_outbox_dispatch` ON `outbox_events` (`published_at`,`dead_at`,`available_at`,`id`);
