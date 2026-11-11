package main

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestSQLitePoolSerializesReadThenWriteTransactions(t *testing.T) {
	db, err := sql.Open("sqlite3", sqliteDSN(filepath.Join(t.TempDir(), "contention.db")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := configureDatabasePool(db, "sqlite", func(string) string { return "" }); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE counters (id INTEGER PRIMARY KEY, n INTEGER); INSERT INTO counters VALUES (1,0)"); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRow("SELECT n FROM counters WHERE id=1").Scan(&n); err != nil {
		t.Fatal(err)
	}
	// A worker on the same API pool cannot commit while a business transaction
	// holds a read snapshot. Otherwise WAL cannot upgrade that stale snapshot.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, contenderErr := db.ExecContext(ctx, "UPDATE counters SET n=n+1 WHERE id=1")
	if !errors.Is(contenderErr, context.DeadlineExceeded) {
		_, upgradeErr := tx.Exec("UPDATE counters SET n=n+1 WHERE id=1")
		t.Fatalf("competing writer bypassed pool: contender=%v snapshot_upgrade=%v", contenderErr, upgradeErr)
	}
	if _, err := tx.Exec("UPDATE counters SET n=n+1 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE counters SET n=n+1 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT n FROM counters WHERE id=1").Scan(&n); err != nil || n != 2 {
		t.Fatalf("serialized writes lost: n=%d err=%v", n, err)
	}
}

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
