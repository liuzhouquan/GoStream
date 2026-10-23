package platform

import "gorm.io/gorm"

// MigrateSchema remains for test fixtures and comparison with the legacy
// AutoMigrate database. Production startup uses frozen SQL in migrations.
func MigrateSchema(db *gorm.DB) error {
	return db.AutoMigrate(
		&User{}, &MediaAsset{}, &UploadSession{}, &UploadChunk{}, &Video{},
		&VideoProcessJob{}, &Follow{}, &VideoAction{}, &Comment{}, &Notification{},
		&ChatMessage{}, &MessageDeviceAck{}, &MessageDevice{}, &ChatGroup{},
		&GroupMember{}, &GroupMessage{}, &GroupRead{}, &GroupDeviceRead{},
		&OutboxEvent{}, &SchemaMigration{},
	)
}
