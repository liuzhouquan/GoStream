-- Durable bounded fanout and per-viewer inbox for following Feed.
CREATE TABLE IF NOT EXISTS `feed_fanout_jobs` (`id` integer PRIMARY KEY AUTOINCREMENT,`video_id` integer NOT NULL,`author_id` integer NOT NULL,`last_follower_id` integer NOT NULL DEFAULT 0,`attempts` integer NOT NULL DEFAULT 0,`available_at` datetime NOT NULL,`lease_until` datetime,`done_at` datetime,`last_error` text,`created_at` datetime);
CREATE UNIQUE INDEX IF NOT EXISTS `idx_feed_fanout_jobs_video_id` ON `feed_fanout_jobs` (`video_id`);
CREATE INDEX IF NOT EXISTS `idx_feed_fanout_dispatch` ON `feed_fanout_jobs` (`done_at`,`available_at`,`lease_until`,`id`);
CREATE TABLE IF NOT EXISTS `feed_inboxes` (`id` integer PRIMARY KEY AUTOINCREMENT,`user_id` integer NOT NULL,`video_id` integer NOT NULL,`created_at` datetime NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS `ux_feed_inbox` ON `feed_inboxes` (`user_id`,`video_id`);
CREATE INDEX IF NOT EXISTS `idx_feed_inbox_user` ON `feed_inboxes` (`user_id`,`created_at` DESC,`video_id` DESC);
