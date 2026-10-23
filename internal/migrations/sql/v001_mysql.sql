-- Frozen v001 MySQL 8.4 schema from GoStream models on 2026-10-24.
CREATE TABLE IF NOT EXISTS `users` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `username` varchar(32) NOT NULL,
  `password_hash` longtext NOT NULL,
  `nickname` varchar(48) NOT NULL,
  `bio` varchar(280) DEFAULT NULL,
  `avatar_url` varchar(512) DEFAULT NULL,
  `last_seen_at` datetime(3) DEFAULT NULL,
  `created_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `idx_users_username` (`username`),
  KEY `idx_users_avatar_url` (`avatar_url`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS `media_assets` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `owner_id` bigint unsigned NOT NULL,
  `url` varchar(512) NOT NULL,
  `kind` varchar(16) NOT NULL,
  `size` bigint NOT NULL,
  `created_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `idx_media_assets_url` (`url`),
  KEY `idx_media_assets_created_at` (`created_at`),
  KEY `idx_media_assets_owner_id` (`owner_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS `upload_sessions` (
  `id` varchar(32) NOT NULL,
  `owner_id` bigint unsigned NOT NULL,
  `ext` varchar(8) NOT NULL,
  `size` bigint NOT NULL,
  `sha256` varchar(64) DEFAULT NULL,
  `chunk_size` bigint NOT NULL,
  `status` varchar(16) NOT NULL DEFAULT 'open',
  `lease_until` datetime(3) DEFAULT NULL,
  `cleaned_at` datetime(3) DEFAULT NULL,
  `expires_at` datetime(3) DEFAULT NULL,
  `created_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_upload_sessions_owner_id` (`owner_id`),
  KEY `idx_upload_sessions_expires_at` (`expires_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS `upload_chunks` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `session_id` varchar(32) NOT NULL,
  `index` bigint NOT NULL,
  `size` bigint NOT NULL,
  `sha256` varchar(64) NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `ux_upload_chunk` (`session_id`,`index`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS `videos` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `author_id` bigint unsigned NOT NULL,
  `title` varchar(120) NOT NULL,
  `description` varchar(2000) DEFAULT NULL,
  `media_url` varchar(512) NOT NULL,
  `cover_url` varchar(512) DEFAULT NULL,
  `status` varchar(16) NOT NULL DEFAULT 'published',
  `like_count` bigint DEFAULT NULL,
  `comment_count` bigint DEFAULT NULL,
  `favorite_count` bigint DEFAULT NULL,
  `play_count` bigint DEFAULT NULL,
  `duration_ms` bigint NOT NULL DEFAULT '0',
  `width` bigint NOT NULL DEFAULT '0',
  `height` bigint NOT NULL DEFAULT '0',
  `created_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_videos_media_url` (`media_url`),
  KEY `idx_videos_cover_url` (`cover_url`),
  KEY `idx_video_feed` (`status`,`created_at` DESC),
  KEY `idx_video_author` (`author_id`,`created_at` DESC),
  CONSTRAINT `fk_videos_author` FOREIGN KEY (`author_id`) REFERENCES `users` (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS `video_process_jobs` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `video_id` bigint unsigned NOT NULL,
  `source_url` varchar(512) NOT NULL,
  `attempts` bigint NOT NULL DEFAULT '0',
  `available_at` datetime(3) NOT NULL,
  `lease_until` datetime(3) DEFAULT NULL,
  `done_at` datetime(3) DEFAULT NULL,
  `dead_at` datetime(3) DEFAULT NULL,
  `last_error` varchar(500) DEFAULT NULL,
  `failure_code` varchar(32) NOT NULL DEFAULT '',
  `created_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_video_process_jobs_video_id` (`video_id`),
  KEY `idx_video_process_jobs_source_url` (`source_url`),
  KEY `idx_video_process_jobs_available_at` (`available_at`),
  KEY `idx_video_process_jobs_lease_until` (`lease_until`),
  KEY `idx_video_process_jobs_done_at` (`done_at`),
  KEY `idx_video_process_jobs_dead_at` (`dead_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS `follows` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `follower_id` bigint unsigned NOT NULL,
  `followee_id` bigint unsigned NOT NULL,
  `created_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `ux_follow_pair` (`follower_id`,`followee_id`),
  KEY `idx_followers` (`followee_id`,`follower_id`),
  KEY `idx_following` (`created_at` DESC)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS `video_actions` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `user_id` bigint unsigned NOT NULL,
  `video_id` bigint unsigned NOT NULL,
  `kind` varchar(16) NOT NULL,
  `created_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `ux_action` (`user_id`,`video_id`,`kind`),
  KEY `idx_action_recent` (`user_id`,`id` DESC),
  KEY `idx_video_actions_video_id` (`video_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS `comments` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `video_id` bigint unsigned NOT NULL,
  `user_id` bigint unsigned NOT NULL,
  `parent_id` bigint unsigned DEFAULT NULL,
  `body` varchar(2000) NOT NULL,
  `created_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_comment_thread` (`video_id`,`parent_id`,`created_at` DESC,`id` DESC),
  KEY `idx_comment_page` (`video_id`,`created_at` DESC),
  KEY `fk_comments_user` (`user_id`),
  CONSTRAINT `fk_comments_user` FOREIGN KEY (`user_id`) REFERENCES `users` (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS `notifications` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `user_id` bigint unsigned NOT NULL,
  `event_key` varchar(100) NOT NULL,
  `kind` varchar(20) NOT NULL,
  `actor_id` bigint unsigned DEFAULT NULL,
  `video_id` bigint unsigned DEFAULT NULL,
  `text` varchar(280) DEFAULT NULL,
  `read_at` datetime(3) DEFAULT NULL,
  `created_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `ux_notification_event` (`user_id`,`event_key`),
  KEY `idx_notification_page` (`user_id`,`created_at` DESC),
  KEY `idx_notification_user_id` (`user_id`,`id` DESC),
  KEY `idx_notification_unread` (`user_id`,`read_at`,`id` DESC),
  KEY `fk_notifications_actor` (`actor_id`),
  CONSTRAINT `fk_notifications_actor` FOREIGN KEY (`actor_id`) REFERENCES `users` (`id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS `chat_messages` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `client_id` varchar(80) NOT NULL,
  `sender_id` bigint unsigned NOT NULL,
  `recipient_id` bigint unsigned NOT NULL,
  `body` varchar(4000) NOT NULL,
  `created_at` datetime(3) DEFAULT NULL,
  `acked_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `ux_message_client` (`client_id`,`sender_id`),
  KEY `idx_message_unacked` (`recipient_id`,`acked_at`),
  KEY `idx_message_thread_id` (`sender_id`,`recipient_id`,`id` DESC),
  KEY `idx_message_thread` (`sender_id`,`recipient_id`,`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS `message_device_acks` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `user_id` bigint unsigned NOT NULL,
  `device_id` varchar(36) NOT NULL,
  `message_id` bigint unsigned NOT NULL,
  `created_at` datetime(3) NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `ux_message_device_ack` (`user_id`,`device_id`,`message_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS `message_devices` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `user_id` bigint unsigned NOT NULL,
  `device_id` varchar(36) NOT NULL,
  `created_at` datetime(3) NOT NULL,
  `last_seen_at` datetime(3) NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `ux_message_device` (`user_id`,`device_id`),
  KEY `idx_message_devices_last_seen_at` (`last_seen_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS `chat_groups` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `owner_id` bigint unsigned NOT NULL,
  `name` varchar(80) NOT NULL,
  `created_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  KEY `idx_chat_groups_owner_id` (`owner_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS `group_members` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `group_id` bigint unsigned NOT NULL,
  `user_id` bigint unsigned NOT NULL,
  `joined_at` datetime(3) NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `ux_group_member` (`group_id`,`user_id`),
  KEY `idx_group_members_user_id` (`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS `group_messages` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `group_id` bigint unsigned NOT NULL,
  `sender_id` bigint unsigned NOT NULL,
  `client_id` varchar(80) NOT NULL,
  `body` varchar(4000) NOT NULL,
  `created_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `ux_group_client` (`sender_id`,`client_id`),
  KEY `idx_group_message` (`group_id`,`created_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS `group_reads` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `group_id` bigint unsigned NOT NULL,
  `user_id` bigint unsigned NOT NULL,
  `last_ack_id` bigint unsigned NOT NULL DEFAULT '0',
  PRIMARY KEY (`id`),
  UNIQUE KEY `ux_group_read` (`group_id`,`user_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS `group_device_reads` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `group_id` bigint unsigned NOT NULL,
  `user_id` bigint unsigned NOT NULL,
  `device_id` varchar(36) NOT NULL,
  `last_ack_id` bigint unsigned NOT NULL DEFAULT '0',
  PRIMARY KEY (`id`),
  UNIQUE KEY `ux_group_device_read` (`group_id`,`user_id`,`device_id`),
  KEY `idx_group_device_owner` (`user_id`,`device_id`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS `outbox_events` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `event_key` varchar(100) NOT NULL,
  `topic` varchar(60) NOT NULL,
  `payload` text NOT NULL,
  `attempts` bigint NOT NULL DEFAULT '0',
  `available_at` datetime(3) DEFAULT NULL,
  `published_at` datetime(3) DEFAULT NULL,
  `dead_at` datetime(3) DEFAULT NULL,
  `last_error` varchar(500) DEFAULT NULL,
  `created_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `idx_outbox_events_event_key` (`event_key`),
  KEY `idx_outbox_events_available_at` (`available_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;

CREATE TABLE IF NOT EXISTS `schema_migrations` (
  `key` varchar(80) NOT NULL,
  `applied_at` datetime(3) DEFAULT NULL,
  PRIMARY KEY (`key`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_0900_ai_ci;
