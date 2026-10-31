CREATE TABLE IF NOT EXISTS `message_device_ack_ranges` (
  `id` bigint unsigned NOT NULL AUTO_INCREMENT,
  `user_id` bigint unsigned NOT NULL,
  `device_id` varchar(36) NOT NULL,
  `first_message_id` bigint unsigned NOT NULL,
  `last_message_id` bigint unsigned NOT NULL,
  PRIMARY KEY (`id`),
  UNIQUE KEY `ux_device_ack_range` (`user_id`,`device_id`,`first_message_id`)
);
