package platform

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// This is an opt-in SQL profile, not an HTTP or ACK-write throughput benchmark.
// Rows live only in a rolled-back transaction in an already migrated test DB.
func TestMySQLDeviceAckQueryProfile(t *testing.T) {
	if os.Getenv("GOSTREAM_TEST_ACK_QUERY_PROFILE") != "1" {
		t.Skip("set GOSTREAM_TEST_ACK_QUERY_PROFILE=1 for the sparse ACK query profile")
	}
	dsn := os.Getenv("GOSTREAM_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Fatal("profile requires an isolated migrated GOSTREAM_TEST_MYSQL_DSN")
	}
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pool.Close(); err != nil {
			t.Error(err)
		}
	})
	var version string
	if err := db.Raw("SELECT VERSION()").Scan(&version).Error; err != nil {
		t.Fatal(err)
	}
	t.Logf("environment go=%s os=%s arch=%s cpu=%d mysql=%s", runtime.Version(), runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), version)
	var settings struct {
		BufferPool uint64
		Isolation  string
		Optimizer  string
	}
	if err := db.Raw("SELECT @@innodb_buffer_pool_size AS buffer_pool, @@transaction_isolation AS isolation, @@optimizer_switch AS optimizer").Scan(&settings).Error; err != nil {
		t.Fatal(err)
	}
	t.Logf("mysql settings=%+v", settings)

	for _, size := range []int{1000, 10000} {
		t.Run(fmt.Sprintf("ranges_%d", size), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			tx := db.WithContext(ctx).Begin()
			if tx.Error != nil {
				t.Fatal(tx.Error)
			}
			defer func() {
				if err := tx.Rollback().Error; err != nil {
					t.Error(err)
				}
			}()
			prefix := fmt.Sprintf("ack_query_%x", time.Now().UnixNano())
			users := []User{{Username: prefix + "s", PasswordHash: "fixture"}, {Username: prefix + "r", PasswordHash: "fixture"}}
			if err := tx.Create(&users).Error; err != nil {
				t.Fatal(err)
			}
			messages := make([]ChatMessage, 4*size+1)
			for i := range messages {
				messages[i] = ChatMessage{SenderID: users[0].ID, RecipientID: users[1].ID, ClientID: fmt.Sprintf("%s_%d", prefix, i), Body: "query fixture"}
			}
			if err := tx.CreateInBatches(&messages, 200).Error; err != nil {
				t.Fatal(err)
			}
			ranges := make([]MessageDeviceAckRange, 0, size+2)
			exact := make([]MessageDeviceAck, 0, size)
			unread := []uint{messages[0].ID}
			for i := 1; i < len(messages); i += 4 {
				if messages[i+1].ID != messages[i].ID+1 {
					t.Fatal("profile requires adjacent allocated IDs; use an isolated database")
				}
				ranges = append(ranges, MessageDeviceAckRange{UserID: users[1].ID, DeviceID: testDeviceA, FirstMessageID: messages[i].ID, LastMessageID: messages[i+1].ID})
				exact = append(exact, MessageDeviceAck{UserID: users[1].ID, DeviceID: testDeviceA, MessageID: messages[i+2].ID})
				unread = append(unread, messages[i+3].ID)
			}
			// Broad ranges for another device and another user must not hide any A gaps.
			for _, scope := range []struct {
				user   uint
				device string
			}{{users[1].ID, testDeviceB}, {users[0].ID, testDeviceA}} {
				ranges = append(ranges, MessageDeviceAckRange{UserID: scope.user, DeviceID: scope.device, FirstMessageID: messages[0].ID, LastMessageID: messages[len(messages)-1].ID})
			}
			if err := tx.CreateInBatches(&ranges, 200).Error; err != nil {
				t.Fatal(err)
			}
			if err := tx.CreateInBatches(&exact, 200).Error; err != nil {
				t.Fatal(err)
			}
			s := &Server{DB: tx}
			for _, page := range []struct {
				name  string
				after uint
			}{{"first", 0}, {"middle", messages[len(messages)/2].ID}, {"tail", messages[len(messages)-401].ID}, {"exhausted", messages[len(messages)-1].ID}} {
				t.Run(page.name, func(t *testing.T) {
					want := make([]uint, 0, 101)
					for _, id := range unread {
						if id > page.after && len(want) < 101 {
							want = append(want, id)
						}
					}
					variants := []struct {
						name  string
						query func() *gorm.DB
					}{
						{"runtime", nil},
						{"v005", func() *gorm.DB {
							return ackQueryProfileBase(tx, users[1].ID).Where("NOT EXISTS (SELECT 1 FROM message_device_ack_ranges r WHERE r.user_id = ? AND r.device_id = ? AND chat_messages.id BETWEEN r.first_message_id AND r.last_message_id)", users[1].ID, testDeviceA)
						}},
						{"predecessor", func() *gorm.DB {
							return ackQueryProfileBase(tx, users[1].ID).Where("COALESCE((SELECT r.last_message_id FROM message_device_ack_ranges r WHERE r.user_id = ? AND r.device_id = ? AND r.first_message_id <= chat_messages.id ORDER BY r.first_message_id DESC LIMIT 1), 0) < chat_messages.id", users[1].ID, testDeviceA)
						}},
					}
					for round := 0; round < 4; round++ {
						for offset := range len(variants) {
							v := variants[(round+offset)%len(variants)]
							var rows []ChatMessage
							var q *gorm.DB
							var err error
							recorder := &ackProfileLogger{Interface: logger.Default.LogMode(logger.Silent)}
							start := time.Now()
							if v.name == "runtime" {
								reader := s
								if round == 0 {
									reader = &Server{DB: tx.Session(&gorm.Session{Logger: recorder})}
								}
								rows, err = reader.loadPendingDirectMessages(ctx, users[1].ID, testDeviceA, page.after, 101)
							} else {
								q = v.query().Where("chat_messages.id > ?", page.after).Order("chat_messages.id ASC").Limit(101)
								err = q.Find(&rows).Error
							}
							if err != nil {
								t.Fatal(err)
							}
							elapsed := time.Since(start)
							got := make([]uint, len(rows))
							for i, m := range rows {
								got[i] = m.ID
							}
							if !slices.Equal(got, want) {
								t.Fatalf("%s IDs=%v want=%v", v.name, got, want)
							}
							if round > 0 {
								t.Logf("sample ranges=%d page=%s variant=%s round=%d rows=%d ms=%.3f", size, page.name, v.name, round, len(rows), float64(elapsed.Microseconds())/1000)
							}
							if round == 0 {
								if q != nil {
									stmt := q.Session(&gorm.Session{DryRun: true}).Find(&[]ChatMessage{}).Statement
									recorder.queries = []string{tx.Dialector.Explain(stmt.SQL.String(), stmt.Vars...)}
								}
								for i, query := range recorder.queries {
									var plan []string
									if err := tx.Raw("EXPLAIN ANALYZE " + query).Scan(&plan).Error; err != nil {
										t.Fatal(err)
									}
									t.Logf("plan variant=%s query=%d\n%s", v.name, i, plan)
								}
							}
						}
					}
				})
			}
		})
	}
}

// Freeze the pre-optimization read shape for side-by-side measurements only.
func ackQueryProfileBase(db *gorm.DB, user uint) *gorm.DB {
	return db.Model(&ChatMessage{}).Where("chat_messages.recipient_id = ? AND chat_messages.acked_at IS NULL", user).
		Joins("LEFT JOIN message_device_acks ON message_device_acks.message_id = chat_messages.id AND message_device_acks.user_id = ? AND message_device_acks.device_id = ?", user, testDeviceA).
		Where("message_device_acks.id IS NULL").Select("chat_messages.*")
}

// Capture only the production loader's SELECTs, not savepoints or fixture writes.
type ackProfileLogger struct {
	logger.Interface
	queries []string
}

func (l *ackProfileLogger) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	query, _ := fc()
	if strings.HasPrefix(query, "SELECT ") {
		l.queries = append(l.queries, query)
	}
}
