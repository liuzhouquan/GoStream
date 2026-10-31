package migrations

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/liuzhouquan/GoStream/internal/platform"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func TestDeviceAckRangeUpgradePreservesExactReceipts(t *testing.T) {
	db := migrationTestDB(t)
	testDeviceAckRangeUpgrade(t, db)
}

func TestDeviceAckRangeUpgradeRejectsDriftAndRetries(t *testing.T) {
	db := migrationTestDB(t)
	if err := Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := db.Where("`key` = ?", upgradeV005Key).Delete(&platform.SchemaMigration{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("DROP INDEX ux_device_ack_range").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE INDEX ux_device_ack_range ON message_device_ack_ranges(id)").Error; err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), db); err == nil || !strings.Contains(err.Error(), "columns differ") {
		t.Fatalf("drift was accepted: %v", err)
	}
	var count int64
	if err := db.Model(&platform.SchemaMigration{}).Where("`key` = ?", upgradeV005Key).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("failed version marked %d %v", count, err)
	}
	if err := db.Exec("DROP INDEX ux_device_ack_range").Error; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := Apply(context.Background(), db); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMySQLV005UpgradeIntegration(t *testing.T) {
	dsn := os.Getenv("GOSTREAM_TEST_MYSQL_LEGACY_DSN")
	if dsn == "" {
		t.Skip("requires an isolated legacy MySQL database")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	testDeviceAckRangeUpgrade(t, db)
}

func testDeviceAckRangeUpgrade(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := Apply(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrator().DropTable("message_device_ack_ranges"); err != nil {
		t.Fatal(err)
	}
	if err := db.Where("`key` = ?", "schema_v005").Delete(&platform.SchemaMigration{}).Error; err != nil {
		t.Fatal(err)
	}
	receipt := platform.MessageDeviceAck{UserID: 42, DeviceID: "00000000-0000-4000-8000-000000000001", MessageID: 123}
	if err := db.Create(&receipt).Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Delete(&receipt).Error; err != nil {
			t.Error(err)
		}
	})
	for range 2 {
		if err := Apply(context.Background(), db); err != nil {
			t.Fatal(err)
		}
	}
	if !db.Migrator().HasTable(&platform.MessageDeviceAckRange{}) || !db.Migrator().HasIndex(&platform.MessageDeviceAckRange{}, "ux_device_ack_range") {
		t.Fatal("v005 range table/index missing")
	}
	var stored platform.MessageDeviceAck
	if err := db.First(&stored, receipt.ID).Error; err != nil || stored.MessageID != 123 {
		t.Fatalf("upgrade changed receipt: %+v %v", stored, err)
	}
	var count int64
	if err := db.Model(&platform.SchemaMigration{}).Where("`key` = ?", "schema_v005").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("marker count=%d err=%v", count, err)
	}
}
