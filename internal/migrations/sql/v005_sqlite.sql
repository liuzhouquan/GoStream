CREATE TABLE IF NOT EXISTS `message_device_ack_ranges` (`id` integer PRIMARY KEY AUTOINCREMENT,`user_id` integer NOT NULL,`device_id` text NOT NULL,`first_message_id` integer NOT NULL,`last_message_id` integer NOT NULL);
CREATE UNIQUE INDEX IF NOT EXISTS `ux_device_ack_range` ON `message_device_ack_ranges`(`user_id`,`device_id`,`first_message_id`);
