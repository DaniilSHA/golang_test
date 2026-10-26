package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/go-sql-driver/mysql"
)

type Config struct {
	Addr          string
	DSN           string
	JWTSecret     string
	RedisAddr     string
	RedisPassword string
	RedisDB       int
	RedisTimeout  time.Duration
	AuthRPM       int
	AuthBurst     int
	APIRPM        int
	APIBurst      int
	LogLevel      string
}

func Load() (Config, error) {
	c := Config{Addr: value("HTTP_ADDR", ":8080"), JWTSecret: os.Getenv("JWT_SECRET"), RedisAddr: value("REDIS_ADDR", "localhost:6379"), RedisPassword: os.Getenv("REDIS_PASSWORD"), LogLevel: value("LOG_LEVEL", "info")}
	if len(c.JWTSecret) < 32 {
		return c, fmt.Errorf("JWT_SECRET must contain at least 32 bytes")
	}
	raw := os.Getenv("MYSQL_DSN")
	if raw == "" {
		return c, fmt.Errorf("MYSQL_DSN is required")
	}
	dsn, err := mysql.ParseDSN(raw)
	if err != nil {
		return c, fmt.Errorf("invalid MYSQL_DSN: %w", err)
	}
	dsn.ParseTime = true
	dsn.Loc = time.UTC
	if dsn.Params == nil {
		dsn.Params = map[string]string{}
	}
	dsn.Params["time_zone"] = "'+00:00'"
	dsn.Params["charset"] = "utf8mb4"
	if dsn.Timeout == 0 {
		dsn.Timeout = 5 * time.Second
	}
	if dsn.ReadTimeout == 0 {
		dsn.ReadTimeout = 5 * time.Second
	}
	if dsn.WriteTimeout == 0 {
		dsn.WriteTimeout = 5 * time.Second
	}
	c.DSN = dsn.FormatDSN()
	for _, v := range []struct {
		key           string
		fallback, min int
		out           *int
	}{
		{"REDIS_DB", 0, 0, &c.RedisDB}, {"RATE_AUTH_RPM", 10, 1, &c.AuthRPM}, {"RATE_AUTH_BURST", 5, 1, &c.AuthBurst}, {"RATE_API_RPM", 120, 1, &c.APIRPM}, {"RATE_API_BURST", 20, 1, &c.APIBurst},
	} {
		n, err := strconv.Atoi(value(v.key, strconv.Itoa(v.fallback)))
		if err != nil || n < v.min {
			return c, fmt.Errorf("invalid %s", v.key)
		}
		*v.out = n
	}
	c.RedisTimeout, err = time.ParseDuration(value("REDIS_TIMEOUT", "200ms"))
	if err != nil || c.RedisTimeout <= 0 {
		return c, fmt.Errorf("invalid REDIS_TIMEOUT")
	}
	if c.LogLevel != "debug" && c.LogLevel != "info" && c.LogLevel != "warn" && c.LogLevel != "error" {
		return c, fmt.Errorf("invalid LOG_LEVEL")
	}
	return c, nil
}
func value(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return fallback
}
