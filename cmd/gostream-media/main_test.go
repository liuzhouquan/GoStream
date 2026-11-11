package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// A reachable bucket must not hide SQL validation or local-file errors. The S3
// test server only answers bucket discovery; no object should be copied here.
func TestImportLocalPropagatesValidationErrors(t *testing.T) {
	for _, scenario := range []struct{ name, url, want string }{
		{"unsafe_reference", "/media/load-fixture.mp4", "unsafe media URL"},
		{"missing_file", "/media/0123456789abcdef0123456789abcdef.mp4", "inspect local"},
		{"missing_table", "", "read url media references"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && r.URL.Query().Has("location") {
					w.Header().Set("Content-Type", "application/xml")
					_, _ = w.Write([]byte(`<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">us-east-1</LocationConstraint>`))
					return
				}
				if r.Method != http.MethodHead {
					t.Errorf("unexpected object operation: %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()
			directory := t.TempDir()
			dsn := filepath.Join(directory, "source.db")
			db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			sqlDB, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			defer sqlDB.Close()
			if scenario.url != "" {
				// ImportLocalMedia only needs the URL projection from this table.
				if err := db.Exec("CREATE TABLE media_assets(url TEXT)").Error; err != nil {
					t.Fatal(err)
				}
				if err := db.Exec("INSERT INTO media_assets(url) VALUES (?)", scenario.url).Error; err != nil {
					t.Fatal(err)
				}
				if scenario.name == "missing_file" {
					for _, table := range []string{"videos(media_url TEXT, cover_url TEXT)", "users(avatar_url TEXT)", "video_process_jobs(source_url TEXT, done_at TEXT, dead_at TEXT)", "upload_sessions(status TEXT, expires_at TEXT)"} {
						if err := db.Exec("CREATE TABLE " + table).Error; err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			t.Setenv("DB_DRIVER", "sqlite")
			t.Setenv("DB_DSN", dsn)
			t.Setenv("MINIO_ENDPOINT", strings.TrimPrefix(server.URL, "http://"))
			t.Setenv("MINIO_ACCESS_KEY", "fixture-access")
			t.Setenv("MINIO_SECRET_KEY", "fixture-secret")
			t.Setenv("MINIO_BUCKET", "fixture-media")
			t.Setenv("MINIO_SECURE", "false")
			err = run([]string{"import-local", "--dir", directory})
			if err == nil || !strings.Contains(err.Error(), scenario.want) {
				t.Fatalf("import-local returned %v, want error containing %q", err, scenario.want)
			}
		})
	}
}
