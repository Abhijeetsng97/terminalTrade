// Package sshserver serves the TUI over SSH with pubkey auth and the
// TOTP gate: `ssh trade.tradeapp.in` -> terminal.
package sshserver

import (
	"errors"
	"fmt"
	"strings"

	"github.com/charmbracelet/log"
	"github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"

	"github.com/Abhijeetsng97/terminalTrade/internal/app"
	"github.com/Abhijeetsng97/terminalTrade/internal/auth"
	tuiapp "github.com/Abhijeetsng97/terminalTrade/internal/tui"
)

// Server wraps the wish SSH server.
type Server struct {
	App  *app.App
	Gate *auth.Gate
	Addr string
	srv  *ssh.Server

	authorizedKeys []string
}

// SetAuthorizedKeys loads the authorized-keys allowlist (one line
// per key). Call before ListenAndServe; empty list = deny all.
func (s *Server) SetAuthorizedKeys(lines []string) {
	s.authorizedKeys = lines
}

func New(a *app.App, gate *auth.Gate, port int) *Server {
	s := &Server{App: a, Gate: gate, Addr: fmt.Sprintf(":%d", port)}

	s.srv = &ssh.Server{
		Addr: s.Addr,
		// Public key auth: the deployment's authorized keys.
		PublicKeyHandler: func(ctx ssh.Context, key ssh.PublicKey) bool {
			// v1: accept any key that's in the server's authorized
			// keys file; a fixed single-user allowlist configured via
			// deployment (TT_AUTHORIZED_KEYS path, one per line).
			return s.authorized(key)
		},
		Handler: s.handle,
	}
	return s
}

// authorized checks the key against the authorized keys file.
func (s *Server) authorized(key ssh.PublicKey) bool {
	// TT_AUTHORIZED_KEYS handled at config load; a missing file
	// denies all (fail closed) — set the env var.
	if len(s.authorizedKeys) == 0 {
		return false
	}
	wanted := strings.TrimSpace(string(gossh.MarshalAuthorizedKey(key)))
	for _, k := range s.authorizedKeys {
		if strings.TrimSpace(k) == wanted {
			return true
		}
	}
	return false
}

// Start begins listening (blocking).
func (s *Server) ListenAndServe() error {
	return s.srv.ListenAndServe()
}

// handle runs the TOTP gate then the TUI.
func (s *Server) handle(sess ssh.Session) {
	// TOTP gate: 3 strikes -> disconnect
	gs := auth.NewGateSession(s.Gate)

	if !s.gatePrompt(sess, gs) {
		_ = sess.Close()
		return
	}

	// run the TUI bound to this session
	p := tuiapp.NewProgram(s.App, sess, sess)
	if _, err := p.Run(); err != nil {
		log.Error("tui exited", "err", err)
	}
}

// gatePrompt renders the TOTP prompt over the SSH session.
func (s *Server) gatePrompt(sess ssh.Session, gs *auth.GateSession) bool {
	render := func(msg string) {
		_, _ = sess.Write([]byte("\x1b[2J\x1b[H" + msg))
	}
	render("terminalTrade\n\nEnter TOTP code (6 digits), 3 attempts:\n> ")
	buf := make([]byte, 64)
	var code []byte
	for {
		n, err := sess.Read(buf)
		if err != nil {
			return false
		}
		for _, b := range buf[:n] {
			switch {
			case b >= '0' && b <= '9':
				code = append(code, b)
				if len(code) == 6 {
					ok, allowed := gs.Attempt(string(code))
					if ok {
						return true
					}
					if !allowed {
						render("\nToo many attempts. Disconnecting.\n")
						return false
					}
					render(fmt.Sprintf("\nWrong code. %d attempts left.\n> ", 3-gs.Attempts()))
					code = code[:0]
				} else {
					render("terminalTrade\n\nEnter TOTP code (6 digits):\n> " + string(code))
				}
			case b == 3, b == 4: // Ctrl+C / Ctrl+D
				return false
			case b == 13, b == 10:
				if len(code) == 6 {
					ok, allowed := gs.Attempt(string(code))
					if ok {
						return true
					}
					if !allowed {
						render("\nToo many attempts. Disconnecting.\n")
						return false
					}
					render(fmt.Sprintf("\nWrong code. %d attempts left.\n> ", 3-gs.Attempts()))
					code = code[:0]
				}
			case b == 127, b == 8: // backspace
				if len(code) > 0 {
					code = code[:len(code)-1]
					render("terminalTrade\n\nEnter TOTP code (6 digits):\n> " + string(code))
				}
			}
		}
	}
}

// ErrNoAuthorizedKeys is returned when the authorized-keys file is
// missing (fail closed).
var ErrNoAuthorizedKeys = errors.New("no authorized keys configured")