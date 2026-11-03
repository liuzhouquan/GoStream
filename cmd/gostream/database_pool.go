package main

import (
	"database/sql"
	"fmt"
	"strconv"
)

type databasePoolConfig struct {
	MaxOpen int
	MaxIdle int
}

func databasePoolConfigFromEnv(getenv func(string) string) (databasePoolConfig, error) {
	config := databasePoolConfig{MaxOpen: 50, MaxIdle: 25}
	for _, option := range []struct {
		name string
		dest *int
	}{
		{name: "DB_MAX_OPEN_CONNS", dest: &config.MaxOpen},
		{name: "DB_MAX_IDLE_CONNS", dest: &config.MaxIdle},
	} {
		value := getenv(option.name)
		if value == "" {
			continue
		}
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return databasePoolConfig{}, fmt.Errorf("%s must be an integer: %w", option.name, err)
		}
		*option.dest = parsed
	}
	if config.MaxOpen <= 0 {
		return databasePoolConfig{}, fmt.Errorf("DB_MAX_OPEN_CONNS must be greater than zero")
	}
	if config.MaxIdle < 0 {
		return databasePoolConfig{}, fmt.Errorf("DB_MAX_IDLE_CONNS cannot be negative")
	}
	if config.MaxIdle > config.MaxOpen {
		return databasePoolConfig{}, fmt.Errorf("DB_MAX_IDLE_CONNS cannot exceed DB_MAX_OPEN_CONNS")
	}
	return config, nil
}

func (config databasePoolConfig) apply(db *sql.DB) error {
	if db == nil {
		return fmt.Errorf("database pool is nil")
	}
	if config.MaxOpen <= 0 || config.MaxIdle < 0 || config.MaxIdle > config.MaxOpen {
		return fmt.Errorf("invalid database pool limits: max open=%d max idle=%d", config.MaxOpen, config.MaxIdle)
	}
	db.SetMaxOpenConns(config.MaxOpen)
	db.SetMaxIdleConns(config.MaxIdle)
	return nil
}
