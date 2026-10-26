-- Durable bounded fanout and per-viewer inbox for following Feed.
CREATE TABLE IF NOT EXISTS `feed_fanout_jobs` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `video_id` bigint unsigned NOT NULL,
  `author_id` bigint unsigned NOT NULL,
  `last_follower_id` bigint unsigned NOT NULL DEFAULT 0,
  `attempts` bigint NOT NULL DEFAULT 0,
  `available_at` datetime(3) NOT NULL,
  `lease_until` datetime(3) DEFAULT NULL,
  `done_at` datetime(3) DEFAULT NULL,
  `last_error` varchar(500) DEFAULT NULL,
  `created_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `idx_feed_fanout_jobs_video_id` (`video_id`),
  KEY `idx_feed_fanout_dispatch` (`done_at`,`available_at`,`lease_until`,`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
CREATE TABLE IF NOT EXISTS `feed_inboxes` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `user_id` bigint unsigned NOT NULL,
  `video_id` bigint unsigned NOT NULL,
  `created_at` datetime(3) NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `ux_feed_inbox` (`user_id`,`video_id`),
  KEY `idx_feed_inbox_user` (`user_id`,`created_at` DESC,`video_id` DESC)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
