package main

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestDatabasePoolConfigFromEnvDefaultsAndOverrides(t *testing.T) {
	defaults, err := databasePoolConfigFromEnv(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if defaults != (databasePoolConfig{MaxOpen: 50, MaxIdle: 25}) {
		t.Fatalf("default pool config = %+v", defaults)
	}

	values := map[string]string{"DB_MAX_OPEN_CONNS": "8", "DB_MAX_IDLE_CONNS": "4"}
	custom, err := databasePoolConfigFromEnv(func(key string) string { return values[key] })
	if err != nil {
		t.Fatal(err)
	}
	if custom != (databasePoolConfig{MaxOpen: 8, MaxIdle: 4}) {
		t.Fatalf("custom pool config = %+v", custom)
	}
}

func TestDatabasePoolConfigFromEnvRejectsInvalidBounds(t *testing.T) {
	for _, values := range []map[string]string{
		{"DB_MAX_OPEN_CONNS": "0"},
		{"DB_MAX_OPEN_CONNS": "not-a-number"},
		{"DB_MAX_IDLE_CONNS": "-1"},
		{"DB_MAX_OPEN_CONNS": "4", "DB_MAX_IDLE_CONNS": "5"},
	} {
		if _, err := databasePoolConfigFromEnv(func(key string) string { return values[key] }); err == nil {
			t.Fatalf("expected invalid pool configuration to fail: %+v", values)
		}
	}
}

func TestDatabasePoolConfigAppliesOpenAndIdleCaps(t *testing.T) {
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "pool.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	config := databasePoolConfig{MaxOpen: 3, MaxIdle: 1}
	if err := config.apply(db); err != nil {
		t.Fatal(err)
	}

	connections := make([]*sql.Conn, 0, config.MaxOpen)
	for range config.MaxOpen {
		conn, err := db.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		connections = append(connections, conn)
	}
	for _, conn := range connections {
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
	}

	stats := db.Stats()
	if stats.MaxOpenConnections != config.MaxOpen {
		t.Fatalf("max open connections = %d, want %d", stats.MaxOpenConnections, config.MaxOpen)
	}
	if stats.Idle > config.MaxIdle {
		t.Fatalf("idle connections = %d, want at most %d", stats.Idle, config.MaxIdle)
	}
	wantClosed := int64(config.MaxOpen - config.MaxIdle)
	if stats.MaxIdleClosed != wantClosed {
		t.Fatalf("max idle closed = %d, want %d", stats.MaxIdleClosed, wantClosed)
	}
}
