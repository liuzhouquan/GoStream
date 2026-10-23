-- Frozen v001 SQLite schema from GoStream models on 2026-10-24.
CREATE TABLE IF NOT EXISTS `users` (`id` integer PRIMARY KEY AUTOINCREMENT,`username` text NOT NULL,`password_hash` text NOT NULL,`nickname` text NOT NULL,`bio` text,`avatar_url` text,`last_seen_at` datetime,`created_at` datetime);

CREATE TABLE IF NOT EXISTS `media_assets` (`id` integer PRIMARY KEY AUTOINCREMENT,`owner_id` integer NOT NULL,`url` text NOT NULL,`kind` text NOT NULL,`size` integer NOT NULL,`created_at` datetime);

CREATE TABLE IF NOT EXISTS `upload_sessions` (`id` text,`owner_id` integer NOT NULL,`ext` text NOT NULL,`size` integer NOT NULL,`sha256` text,`chunk_size` integer NOT NULL,`status` text NOT NULL DEFAULT "open",`lease_until` datetime,`cleaned_at` datetime,`expires_at` datetime,`created_at` datetime,PRIMARY KEY (`id`));

CREATE TABLE IF NOT EXISTS `upload_chunks` (`id` integer PRIMARY KEY AUTOINCREMENT,`session_id` text NOT NULL,`index` integer NOT NULL,`size` integer NOT NULL,`sha256` text NOT NULL);

CREATE TABLE IF NOT EXISTS `videos` (`id` integer PRIMARY KEY AUTOINCREMENT,`author_id` integer NOT NULL,`title` text NOT NULL,`description` text,`media_url` text NOT NULL,`cover_url` text,`status` text NOT NULL DEFAULT "published",`like_count` integer,`comment_count` integer,`favorite_count` integer,`play_count` integer,`duration_ms` integer NOT NULL DEFAULT 0,`width` integer NOT NULL DEFAULT 0,`height` integer NOT NULL DEFAULT 0,`created_at` datetime,CONSTRAINT `fk_videos_author` FOREIGN KEY (`author_id`) REFERENCES `users`(`id`));

CREATE TABLE IF NOT EXISTS `video_process_jobs` (`id` integer PRIMARY KEY AUTOINCREMENT,`video_id` integer NOT NULL,`source_url` text NOT NULL,`attempts` integer NOT NULL DEFAULT 0,`available_at` datetime NOT NULL,`lease_until` datetime,`done_at` datetime,`dead_at` datetime,`last_error` text,`failure_code` text NOT NULL DEFAULT "",`created_at` datetime);

CREATE TABLE IF NOT EXISTS `follows` (`id` integer PRIMARY KEY AUTOINCREMENT,`follower_id` integer NOT NULL,`followee_id` integer NOT NULL,`created_at` datetime);

CREATE TABLE IF NOT EXISTS `video_actions` (`id` integer PRIMARY KEY AUTOINCREMENT,`user_id` integer NOT NULL,`video_id` integer NOT NULL,`kind` text NOT NULL,`created_at` datetime);

CREATE TABLE IF NOT EXISTS `comments` (`id` integer PRIMARY KEY AUTOINCREMENT,`video_id` integer NOT NULL,`user_id` integer NOT NULL,`parent_id` integer,`body` text NOT NULL,`created_at` datetime,CONSTRAINT `fk_comments_user` FOREIGN KEY (`user_id`) REFERENCES `users`(`id`));

CREATE TABLE IF NOT EXISTS `notifications` (`id` integer PRIMARY KEY AUTOINCREMENT,`user_id` integer NOT NULL,`event_key` text NOT NULL,`kind` text NOT NULL,`actor_id` integer,`video_id` integer,`text` text,`read_at` datetime,`created_at` datetime,CONSTRAINT `fk_notifications_actor` FOREIGN KEY (`actor_id`) REFERENCES `users`(`id`));

CREATE TABLE IF NOT EXISTS `chat_messages` (`id` integer PRIMARY KEY AUTOINCREMENT,`client_id` text NOT NULL,`sender_id` integer NOT NULL,`recipient_id` integer NOT NULL,`body` text NOT NULL,`created_at` datetime,`acked_at` datetime);

CREATE TABLE IF NOT EXISTS `message_device_acks` (`id` integer PRIMARY KEY AUTOINCREMENT,`user_id` integer NOT NULL,`device_id` text NOT NULL,`message_id` integer NOT NULL,`created_at` datetime NOT NULL);

CREATE TABLE IF NOT EXISTS `message_devices` (`id` integer PRIMARY KEY AUTOINCREMENT,`user_id` integer NOT NULL,`device_id` text NOT NULL,`created_at` datetime NOT NULL,`last_seen_at` datetime NOT NULL);

CREATE TABLE IF NOT EXISTS `chat_groups` (`id` integer PRIMARY KEY AUTOINCREMENT,`owner_id` integer NOT NULL,`name` text NOT NULL,`created_at` datetime);

CREATE TABLE IF NOT EXISTS `group_members` (`id` integer PRIMARY KEY AUTOINCREMENT,`group_id` integer NOT NULL,`user_id` integer NOT NULL,`joined_at` datetime NOT NULL);

CREATE TABLE IF NOT EXISTS `group_messages` (`id` integer PRIMARY KEY AUTOINCREMENT,`group_id` integer NOT NULL,`sender_id` integer NOT NULL,`client_id` text NOT NULL,`body` text NOT NULL,`created_at` datetime);

CREATE TABLE IF NOT EXISTS `group_reads` (`id` integer PRIMARY KEY AUTOINCREMENT,`group_id` integer NOT NULL,`user_id` integer NOT NULL,`last_ack_id` integer NOT NULL DEFAULT 0);

CREATE TABLE IF NOT EXISTS `group_device_reads` (`id` integer PRIMARY KEY AUTOINCREMENT,`group_id` integer NOT NULL,`user_id` integer NOT NULL,`device_id` text NOT NULL,`last_ack_id` integer NOT NULL DEFAULT 0);

CREATE TABLE IF NOT EXISTS `outbox_events` (`id` integer PRIMARY KEY AUTOINCREMENT,`event_key` text NOT NULL,`topic` text NOT NULL,`payload` text NOT NULL,`attempts` integer NOT NULL DEFAULT 0,`available_at` datetime,`published_at` datetime,`dead_at` datetime,`last_error` text,`created_at` datetime);

CREATE TABLE IF NOT EXISTS `schema_migrations` (`key` text,`applied_at` datetime,PRIMARY KEY (`key`));

CREATE INDEX IF NOT EXISTS `idx_action_recent` ON `video_actions`(`user_id`,`id` desc);
CREATE INDEX IF NOT EXISTS `idx_chat_groups_owner_id` ON `chat_groups`(`owner_id`);
CREATE INDEX IF NOT EXISTS `idx_comment_page` ON `comments`(`video_id`,`created_at` desc);
CREATE INDEX IF NOT EXISTS `idx_comment_thread` ON `comments`(`video_id`,`parent_id`,`created_at` desc,`id` desc);
CREATE INDEX IF NOT EXISTS `idx_followers` ON `follows`(`followee_id`,`follower_id`);
CREATE INDEX IF NOT EXISTS `idx_following` ON `follows`(`created_at` desc);
CREATE INDEX IF NOT EXISTS `idx_group_device_owner` ON `group_device_reads`(`user_id`,`device_id`);
CREATE INDEX IF NOT EXISTS `idx_group_members_user_id` ON `group_members`(`user_id`);
CREATE INDEX IF NOT EXISTS `idx_group_message` ON `group_messages`(`group_id`,`created_at`);
CREATE INDEX IF NOT EXISTS `idx_media_assets_created_at` ON `media_assets`(`created_at`);
CREATE INDEX IF NOT EXISTS `idx_media_assets_owner_id` ON `media_assets`(`owner_id`);
CREATE UNIQUE INDEX IF NOT EXISTS `idx_media_assets_url` ON `media_assets`(`url`);
CREATE INDEX IF NOT EXISTS `idx_message_devices_last_seen_at` ON `message_devices`(`last_seen_at`);
CREATE INDEX IF NOT EXISTS `idx_message_thread` ON `chat_messages`(`sender_id`,`recipient_id`,`created_at`);
CREATE INDEX IF NOT EXISTS `idx_message_thread_id` ON `chat_messages`(`sender_id`,`recipient_id`,`id` desc);
CREATE INDEX IF NOT EXISTS `idx_message_unacked` ON `chat_messages`(`recipient_id`,`acked_at`);
CREATE INDEX IF NOT EXISTS `idx_notification_page` ON `notifications`(`user_id`,`created_at` desc);
CREATE INDEX IF NOT EXISTS `idx_notification_unread` ON `notifications`(`user_id`,`read_at`,`id` desc);
CREATE INDEX IF NOT EXISTS `idx_notification_user_id` ON `notifications`(`user_id`,`id` desc);
CREATE INDEX IF NOT EXISTS `idx_outbox_events_available_at` ON `outbox_events`(`available_at`);
CREATE UNIQUE INDEX IF NOT EXISTS `idx_outbox_events_event_key` ON `outbox_events`(`event_key`);
CREATE INDEX IF NOT EXISTS `idx_upload_sessions_expires_at` ON `upload_sessions`(`expires_at`);
CREATE INDEX IF NOT EXISTS `idx_upload_sessions_owner_id` ON `upload_sessions`(`owner_id`);
CREATE INDEX IF NOT EXISTS `idx_users_avatar_url` ON `users`(`avatar_url`);
CREATE UNIQUE INDEX IF NOT EXISTS `idx_users_username` ON `users`(`username`);
CREATE INDEX IF NOT EXISTS `idx_video_actions_video_id` ON `video_actions`(`video_id`);
CREATE INDEX IF NOT EXISTS `idx_video_author` ON `videos`(`author_id`,`created_at` desc);
CREATE INDEX IF NOT EXISTS `idx_video_feed` ON `videos`(`status`,`created_at` desc);
CREATE INDEX IF NOT EXISTS `idx_video_process_jobs_available_at` ON `video_process_jobs`(`available_at`);
CREATE INDEX IF NOT EXISTS `idx_video_process_jobs_dead_at` ON `video_process_jobs`(`dead_at`);
CREATE INDEX IF NOT EXISTS `idx_video_process_jobs_done_at` ON `video_process_jobs`(`done_at`);
CREATE INDEX IF NOT EXISTS `idx_video_process_jobs_lease_until` ON `video_process_jobs`(`lease_until`);
CREATE INDEX IF NOT EXISTS `idx_video_process_jobs_source_url` ON `video_process_jobs`(`source_url`);
CREATE INDEX IF NOT EXISTS `idx_video_process_jobs_video_id` ON `video_process_jobs`(`video_id`);
CREATE INDEX IF NOT EXISTS `idx_videos_cover_url` ON `videos`(`cover_url`);
CREATE INDEX IF NOT EXISTS `idx_videos_media_url` ON `videos`(`media_url`);
CREATE UNIQUE INDEX IF NOT EXISTS `ux_action` ON `video_actions`(`user_id`,`video_id`,`kind`);
CREATE UNIQUE INDEX IF NOT EXISTS `ux_follow_pair` ON `follows`(`follower_id`,`followee_id`);
CREATE UNIQUE INDEX IF NOT EXISTS `ux_group_client` ON `group_messages`(`sender_id`,`client_id`);
CREATE UNIQUE INDEX IF NOT EXISTS `ux_group_device_read` ON `group_device_reads`(`group_id`,`user_id`,`device_id`);
CREATE UNIQUE INDEX IF NOT EXISTS `ux_group_member` ON `group_members`(`group_id`,`user_id`);
CREATE UNIQUE INDEX IF NOT EXISTS `ux_group_read` ON `group_reads`(`group_id`,`user_id`);
CREATE UNIQUE INDEX IF NOT EXISTS `ux_message_client` ON `chat_messages`(`client_id`,`sender_id`);
CREATE UNIQUE INDEX IF NOT EXISTS `ux_message_device` ON `message_devices`(`user_id`,`device_id`);
CREATE UNIQUE INDEX IF NOT EXISTS `ux_message_device_ack` ON `message_device_acks`(`user_id`,`device_id`,`message_id`);
CREATE UNIQUE INDEX IF NOT EXISTS `ux_notification_event` ON `notifications`(`user_id`,`event_key`);
CREATE UNIQUE INDEX IF NOT EXISTS `ux_upload_chunk` ON `upload_chunks`(`session_id`,`index`);
