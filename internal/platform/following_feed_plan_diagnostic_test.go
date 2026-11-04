package platform

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// TestA04FollowingQueryPlanDiagnostic is called explicitly by the A04 runner
// after its measured and Performance Schema snapshot windows. It captures the
// production query builder's first-page plan without adding EXPLAIN work to the
// request workload or production binary.
func TestA04FollowingQueryPlanDiagnostic(t *testing.T) {
	dsn := os.Getenv("GOSTREAM_TEST_MYSQL_DSN")
	mode := os.Getenv("GOSTREAM_A04_PLAN_MODE")
	viewerValue := os.Getenv("GOSTREAM_A04_PLAN_VIEWER_ID")
	outputPath := os.Getenv("GOSTREAM_A04_PLAN_OUTPUT")
	if dsn == "" || mode == "" || viewerValue == "" || outputPath == "" {
		t.Skip("set A04 MySQL plan diagnostic environment to capture a Following query plan")
	}
	if mode != "sql-only" && mode != "hybrid" {
		t.Fatalf("unsupported A04 plan mode %q", mode)
	}
	viewerID64, err := strconv.ParseUint(viewerValue, 10, strconv.IntSize)
	if err != nil || viewerID64 == 0 {
		t.Fatalf("invalid A04 plan viewer id %q", viewerValue)
	}
	viewerID := uint(viewerID64)

	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()

	hybrid := mode == "hybrid"
	query := db.Session(&gorm.Session{DryRun: true}).Model(&Video{}).Where("videos.status = ?", "published")
	query = followingCandidateQuery(query, viewerID, feedCursor{}, 20, hybrid)
	var videos []Video
	statement := query.Limit(21).Find(&videos).Statement
	if statement.Error != nil {
		t.Fatal(statement.Error)
	}
	if statement.SQL.Len() == 0 {
		t.Fatal("production Following query builder produced no SQL")
	}

	var planText string
	if err := db.Raw("EXPLAIN FORMAT=JSON "+statement.SQL.String(), statement.Vars...).Row().Scan(&planText); err != nil {
		t.Fatalf("explain production Following query: %v", err)
	}
	if !json.Valid([]byte(planText)) {
		t.Fatalf("MySQL returned invalid EXPLAIN JSON")
	}

	report := struct {
		Source   string          `json:"source"`
		Mode     string          `json:"mode"`
		ViewerID uint            `json:"viewer_id"`
		PageSize int             `json:"page_size"`
		Plan     json.RawMessage `json:"plan"`
	}{
		Source:   "production followingCandidateQuery; first page; limit 20 plus one row",
		Mode:     mode,
		ViewerID: viewerID,
		PageSize: 20,
		Plan:     json.RawMessage(planText),
	}
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outputPath, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("captured A04 %s Following query plan", mode)
}
