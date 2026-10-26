-- Retire completed ordinary-author posts from the indexed pull branch.
ALTER TABLE `videos` ADD COLUMN `feed_pull` numeric NOT NULL DEFAULT true;
CREATE INDEX `idx_video_feed_pull` ON `videos` (`status`,`feed_pull`,`created_at` DESC,`id` DESC);
