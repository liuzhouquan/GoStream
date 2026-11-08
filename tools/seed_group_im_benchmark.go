//go:build ignore

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/liuzhouquan/GoStream/internal/platform"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/driver/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type userSummary struct {
	ID       uint   `json:"id"`
	Username string `json:"username"`
}

func main() {
	driver := flag.String("driver", "mysql", "database driver: mysql or sqlite")
	dsn := flag.String("dsn", "", "database DSN")
	action := flag.String("action", "seed", "action: seed or verify")
	verifyGroup := flag.Uint("verify-group", 0, "group ID to verify")
	expectedCount := flag.Int("expected-count", 0, "expected message count in verify")
	clientCount := flag.Int("count", 20, "number of benchmark accounts to seed")
	flag.Parse()

	dbDSN := *dsn
	if dbDSN == "" {
		if *driver == "mysql" {
			dbDSN = os.Getenv("GOSTREAM_TEST_MYSQL_DSN")
			if dbDSN == "" {
				mysqlAddr := os.Getenv("GOSTREAM_MYSQL_ADDR")
				if mysqlAddr == "" {
					mysqlAddr = "127.0.0.1:13307"
				}
				dbDSN = fmt.Sprintf("gostream:gostream-local@tcp(%s)/gostream?charset=utf8mb4&parseTime=True&loc=Local", mysqlAddr)
			}
		} else {
			dbDSN = "group_im.db"
		}
	}

	var dialector gorm.Dialector
	if *driver == "sqlite" {
		dialector = sqlite.Open(dbDSN)
	} else {
		dialector = mysql.Open(dbDSN)
	}

	db, err := gorm.Open(dialector, &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		log.Fatalf("failed to connect to database (%s: %s): %v", *driver, sanitizeDSN(dbDSN), err)
	}

	if *action == "verify" {
		if *verifyGroup == 0 {
			log.Fatalf("verify-group must be greater than 0")
		}
		var rows []struct {
			ID       uint   `gorm:"column:id"`
			ClientID string `gorm:"column:client_id"`
		}
		if err := db.Table("group_messages").
			Select("id, client_id").
			Where("group_id = ?", *verifyGroup).
			Order("id ASC").
			Scan(&rows).Error; err != nil {
			log.Fatalf("failed to query group_messages: %v", err)
		}

		seen := make(map[string]bool, len(rows))
		var prevID uint
		for _, r := range rows {
			if r.ID <= prevID {
				log.Fatalf("non strictly increasing id: %d <= %d", r.ID, prevID)
			}
			prevID = r.ID
			if seen[r.ClientID] {
				log.Fatalf("duplicate client_id in db: %s", r.ClientID)
			}
			seen[r.ClientID] = true
		}

		if *expectedCount > 0 && len(rows) != *expectedCount {
			log.Fatalf("expected %d rows in SQL, got %d", *expectedCount, len(rows))
		}

		out, _ := json.Marshal(map[string]any{
			"status":   "ok",
			"group_id": *verifyGroup,
			"rows":     len(rows),
		})
		fmt.Printf("SQL_VERIFY_GROUP_OK %s\n", string(out))
		return
	}

	// Action == "seed"
	now := time.Now().UTC().Truncate(time.Second)
	pwHash, err := bcrypt.GenerateFromPassword([]byte("Password123!"), bcrypt.DefaultCost)
	if err != nil {
		log.Fatalf("bcrypt hash password: %v", err)
	}

	var summaries []userSummary
	err = db.Transaction(func(tx *gorm.DB) error {
		// Clean up existing benchmark records if any
		var oldUsers []platform.User
		if err := tx.Where("username LIKE ?", "gfanout_u%").Find(&oldUsers).Error; err == nil && len(oldUsers) > 0 {
			oldIDs := make([]uint, len(oldUsers))
			for i, u := range oldUsers {
				oldIDs[i] = u.ID
			}
			_ = tx.Where("user_id IN ?", oldIDs).Delete(&platform.GroupMember{}).Error
			_ = tx.Where("user_id IN ?", oldIDs).Delete(&platform.GroupDeviceRead{}).Error
			_ = tx.Where("sender_id IN ?", oldIDs).Delete(&platform.GroupMessage{}).Error
			_ = tx.Where("user_id IN ?", oldIDs).Delete(&platform.MessageDevice{}).Error
			_ = tx.Where("id IN ?", oldIDs).Delete(&platform.User{}).Error
		}

		users := make([]platform.User, *clientCount)
		for i := 0; i < *clientCount; i++ {
			users[i] = platform.User{
				Username:     fmt.Sprintf("gfanout_u%02d", i+1),
				PasswordHash: string(pwHash),
				Nickname:     fmt.Sprintf("Nick_%02d", i+1),
				CreatedAt:    now.Add(-time.Duration(*clientCount-i) * time.Minute),
			}
		}
		if err := tx.Create(&users).Error; err != nil {
			return fmt.Errorf("create benchmark users: %w", err)
		}

		summaries = make([]userSummary, len(users))
		for i, u := range users {
			summaries[i] = userSummary{ID: u.ID, Username: u.Username}
		}
		return nil
	})

	if err != nil {
		log.Fatalf("seeding failed: %v", err)
	}

	out, _ := json.Marshal(map[string]any{
		"status": "ok",
		"count":  len(summaries),
		"users":  summaries,
	})
	fmt.Printf("SEED_GROUP_IM_BENCHMARK_OK %s\n", string(out))
}

func sanitizeDSN(raw string) string {
	prefix := ""
	clean := raw
	if idx := strings.Index(clean, "://"); idx != -1 {
		prefix = clean[:idx+3]
		clean = clean[idx+3:]
	}
	atIdx := strings.LastIndex(clean, "@")
	if atIdx == -1 {
		return raw
	}
	colonIdx := strings.Index(clean[:atIdx], ":")
	if colonIdx == -1 {
		return raw
	}
	return prefix + clean[:colonIdx+1] + "***" + clean[atIdx:]
}

