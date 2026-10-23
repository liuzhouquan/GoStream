package platform

import "gorm.io/gorm"

// MigrateSchema defines the first, additive schema baseline. It still reads
// current models; freeze it before adding a second numbered migration.
func MigrateSchema(db *gorm.DB) error {
	return db.AutoMigrate(
		&User{}, &MediaAsset{}, &UploadSession{}, &UploadChunk{}, &Video{},
		&VideoProcessJob{}, &Follow{}, &VideoAction{}, &Comment{}, &Notification{},
		&ChatMessage{}, &MessageDeviceAck{}, &MessageDevice{}, &ChatGroup{},
		&GroupMember{}, &GroupMessage{}, &GroupRead{}, &GroupDeviceRead{},
		&OutboxEvent{}, &SchemaMigration{},
	)
}
