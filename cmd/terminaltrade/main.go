// terminalTrade: SSH trading terminal + REST API for Indian index
// options, multi-broker (Kite + Fyers).
//
// Secrets and knobs come from the environment (.env in dev):
// everything with a TT_ / KITE_ / FYERS_ prefix. See .env.example.
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/charmbracelet/log"

	"github.com/Abhijeetsng97/terminalTrade/internal/api"
	"github.com/Abhijeetsng97/terminalTrade/internal/app"
	"github.com/Abhijeetsng97/terminalTrade/internal/auth"
	"github.com/Abhijeetsng97/terminalTrade/internal/config"
	"github.com/Abhijeetsng97/terminalTrade/internal/sshserver"
	"github.com/Abhijeetsng97/terminalTrade/internal/store"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cfg, err := config.Load()
	if err != nil {
		log.Fatal("config", "err", err)
	}

	// store
	st, err := store.Connect(ctx, cfg.MongoURI, cfg.MongoDB, cfg.EncryptionKey)
	if err != nil {
		log.Fatal("mongo", "err", err)
	}
	defer st.Close(context.Background())

	// app (composition root)
	a, err := app.New(ctx, cfg, st)
	if err != nil {
		log.Fatal("app", "err", err)
	}

	// TOTP gate (first run enrolls: secret persisted to Mongo config)
	gate := setupTOTP(ctx, cfg, st)

	// background ops
	stopOps, err := a.StartOps(ctx)
	if err != nil {
		log.Fatal("ops", "err", err)
	}
	defer stopOps()

	// REST API
	apiSrv := api.New(a, cfg.APIKey)
	go func() {
		addr := fmt.Sprintf("127.0.0.1:%d", cfg.APIPort)
		log.Info("api listening", "addr", addr)
		if err := http.ListenAndServe(addr, apiSrv.Handler()); err != nil {
			log.Error("api", "err", err)
		}
	}()

	// SSH server (the terminal)
	sshSrv, err := sshserver.New(a, gate, fmt.Sprintf(":%d", cfg.SSHPort), cfg.SSHHostKey)
	if err != nil {
		log.Fatal("ssh", "err", err)
	}
	if keys := loadAuthorizedKeys(); len(keys) > 0 {
		sshSrv.SetAuthorizedKeys(keys)
		log.Info("ssh authorized keys loaded", "count", len(keys))
	} else {
		log.Warn("no authorized keys: SSH will deny all logins — set TT_AUTHORIZED_KEYS")
	}
	log.Info("ssh listening", "port", cfg.SSHPort)
	if err := sshSrv.ListenAndServe(); err != nil {
		log.Fatal("ssh", "err", err)
	}
}

// setupTOTP loads or creates the TOTP secret, printing the
// enrollment URL on first run.
func setupTOTP(ctx context.Context, cfg *config.Config, st *store.Store) *auth.Gate {
	secret := cfg.TOTPSecret
	if secret == "" {
		// read from app_config
		if blob, err := st.LoadAppConfig(ctx, "totp_secret"); err == nil && len(blob) > 0 {
			secret = string(blob)
		}
	}
	gate := auth.NewGate(secret)
	if secret == "" {
		// first run: enroll
		newSecret, _, err := gate.EnsureSecret()
		if err != nil {
			log.Fatal("totp", "err", err)
		}
		if err := st.SaveAppConfig(ctx, "totp_secret", []byte(newSecret)); err != nil {
			log.Fatal("totp persist", "err", err)
		}
		log.Warn("TOTP enrolled", "secret", newSecret)
		log.Warn("add to your authenticator:", "url", gate.URL())
	} else {
		_, _, _ = gate.EnsureSecret()
	}
	return gate
}

// loadAuthorizedKeys reads TT_AUTHORIZED_KEYS (path or inline keys).
func loadAuthorizedKeys() []string {
	v := os.Getenv("TT_AUTHORIZED_KEYS")
	if v == "" {
		return nil
	}
	// if it names an existing file, read it; otherwise treat as inline keys
	if data, err := os.ReadFile(v); err == nil {
		return splitLines(string(data))
	}
	return splitLines(v)
}

func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}