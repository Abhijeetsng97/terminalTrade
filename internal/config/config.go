// Package config loads all secrets and knobs from the environment
// (a .env file in development). Nothing reads secrets from anywhere
// else.
package config

import (
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config is the full app configuration.
type Config struct {
	// Server
	SSHPort     int
	SSHHostKey  string // path to host key; generated on first run if missing
	APIPort     int
	APIKey      string // bearer token for the REST API

	// Mongo
	MongoURI  string
	MongoDB   string

	// Encryption master key (32 bytes, hex).
	EncryptionKey []byte

	// Broker apps (from broker developer consoles).
	KiteAPIKey    string
	KiteAPISecret string
	FyersAppID    string
	FyersAppSecret string
	FyersRedirectURI string
	FyersPIN      string // app pin for refresh-token flow

	// TOTP
	TOTPSecret string // base32; generated on first run if empty

	// Trading
	Underlyings []string // chain underlyings to preload (NIFTY, BANKNIFTY)

	// Ops
	InstrumentRefreshCron string // cron spec for background refresh
}

// Load reads the environment, applying defaults. A .env file is
// parsed when present (dev mode); production reads real env vars.
func Load() (*Config, error) {
	// minimal .env support: KEY=VALUE lines, no interpolation
	if _, err := os.Stat(".env"); err == nil {
		if err := loadDotEnv(".env"); err != nil {
			return nil, fmt.Errorf("parsing .env: %w", err)
		}
	}

	c := &Config{
		SSHPort:    envInt("TT_SSH_PORT", 2222),
		APIPort:    envInt("TT_API_PORT", 8080),
		SSHHostKey: envStr("TT_SSH_HOSTKEY", ".ssh_hostkey"),
		APIKey:     envStr("TT_API_KEY", ""),
		MongoURI:   envStr("TT_MONGO_URI", "mongodb://localhost:27017"),
		MongoDB:    envStr("TT_MONGO_DB", "terminaltrade"),

		KiteAPIKey:       envStr("KITE_API_KEY", ""),
		KiteAPISecret:    envStr("KITE_API_SECRET", ""),
		FyersAppID:       envStr("FYERS_APP_ID", ""),
		FyersAppSecret:   envStr("FYERS_APP_SECRET", ""),
		FyersRedirectURI: envStr("FYERS_REDIRECT_URI", ""),
		FyersPIN:         envStr("FYERS_PIN", ""),
		TOTPSecret:      envStr("TT_TOTP_SECRET", ""),

		Underlyings: strings.Split(envStr("TT_UNDERLYINGS", "NIFTY,BANKNIFTY"), ","),
	}
	for i := range c.Underlyings {
		c.Underlyings[i] = strings.TrimSpace(strings.ToUpper(c.Underlyings[i]))
	}

	keyHex := envStr("TT_ENCRYPTION_KEY", "")
	if keyHex == "" {
		return nil, fmt.Errorf("TT_ENCRYPTION_KEY missing: generate with `openssl rand -hex 32`")
	}
	key, err := hex.DecodeString(strings.TrimSpace(keyHex))
	if err != nil {
		return nil, fmt.Errorf("TT_ENCRYPTION_KEY must be hex: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("TT_ENCRYPTION_KEY must decode to 32 bytes, got %d", len(key))
	}
	c.EncryptionKey = key

	return c, nil
}

// loadDotEnv reads KEY=VALUE lines into the process env (existing
// vars win — real env overrides .env).
func loadDotEnv(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.Index(line, "=")
		if eq < 0 {
			continue
		}
		k := strings.TrimSpace(line[:eq])
		v := strings.TrimSpace(line[eq+1:])
		v = strings.Trim(v, `"'`)
		if os.Getenv(k) == "" { // real env wins
			_ = os.Setenv(k, v)
		}
	}
	return nil
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}