// Package sshserver serves the TUI over SSH with pubkey auth and the
// TOTP gate: `ssh trade.tradeapp.in` -> terminal.
//
// Built on wish's official middleware chain: activeterm (rejects
// non-PTY clients) + bubbletea (PTY allocation, window-size
// forwarding, renderer setup) — which is what makes the TUI actually
// render over SSH. The TOTP gate is a custom middleware that runs
// before the bubbletea middleware.
package sshserver

import (
	"errors"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/log"
	"github.com/charmbracelet/ssh"
	"github.com/charmbracelet/wish"
	"github.com/charmbracelet/wish/activeterm"
	bubbleteamw "github.com/charmbracelet/wish/bubbletea"
	gossh "golang.org/x/crypto/ssh"

	"github.com/Abhijeetsng97/terminalTrade/internal/app"
	"github.com/Abhijeetsng97/terminalTrade/internal/auth"
	tuiapp "github.com/Abhijeetsng97/terminalTrade/internal/tui"
)

// ErrNoAuthorizedKeys is returned when no authorized keys are
// configured (fail closed).
var ErrNoAuthorizedKeys = errors.New("no authorized keys configured")

// SetAuthorizedKeys loads the public-key allowlist (one line per
// key, standard authorized_keys format). Call before
// ListenAndServe; empty list = deny all.
func (s *Server) SetAuthorizedKeys(lines []string) {
	s.authorizedKeys = lines
}

// Server wraps the wish-built SSH server. Pubkey allowlisting is
// checked at auth time from authorizedKeys (set via SetAuthorizedKeys
// before ListenAndServe; empty = deny all).
type Server struct {
	App            *app.App
	Gate           *auth.Gate
	Addr           string
	srv            *ssh.Server
	authorizedKeys []string
}

func New(a *app.App, gate *auth.Gate, addr, hostKeyPath string) (*Server, error) {
	s := &Server{App: a, Gate: gate, Addr: addr}

	srv, err := wish.NewServer(
		wish.WithAddress(addr),
		wish.WithHostKeyPath(hostKeyPath),
		wish.WithPublicKeyAuth(s.pubKeyHandler),
		// wish composes middlewares "first to last, last executed
		// first" — so the LAST entry here is the OUTERMOST and runs
		// first. The TOTP gate must be outermost (gate before the TUI),
		// bubbletea innermost (its `next` after the TUI quits is the
		// no-op base handler, so quitting closes the session instead
		// of re-running the gate).
		wish.WithMiddleware(
			bubbleteamw.Middleware(func(sess ssh.Session) (tea.Model, []tea.ProgramOption) {
				m := tuiapp.New(a)
				// MakeOptions wires input/output/renderer; AltScreen
				// makes the TUI take over the full terminal.
				opts := append(bubbleteamw.MakeOptions(sess), tea.WithAltScreen())
				return m, opts
			}),
			activeterm.Middleware(),
			s.totpGateMW(),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("build ssh server: %w", err)
	}
	s.srv = srv
	return s, nil
}

// pubKeyHandler is the SSH pubkey check against the allowlist
// (type + base64 compared; comments ignored). Empty allowlist = deny
// all — fail closed.
func (s *Server) pubKeyHandler(ctx ssh.Context, key ssh.PublicKey) bool {
	if len(s.authorizedKeys) == 0 {
		return false
	}
	wanted := strings.TrimSpace(string(gossh.MarshalAuthorizedKey(key)))
	wf := strings.Fields(wanted)
	if len(wf) >= 2 {
		wanted = wf[0] + " " + wf[1]
	}
	for _, line := range s.authorizedKeys {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) < 2 {
			continue
		}
		if fields[0]+" "+fields[1] == wanted {
			return true
		}
	}
	return false
}

// totpGateMW runs the TOTP challenge before any TUI middleware.
func (s *Server) totpGateMW() wish.Middleware {
	return func(next ssh.Handler) ssh.Handler {
		return func(sess ssh.Session) {
			gs := auth.NewGateSession(s.Gate)
			if !runGatePrompt(sess, gs) {
				_ = sess.CloseWrite() // let the client drain, then it exits
				_ = sess.Close()
				return
			}
			// screen reset before the TUI takes over
			_, _ = fmt.Fprint(sess, "\x1b[2J\x1b[3J\x1b[H")
			next(sess)
		}
	}
}

// runGatePrompt asks for the 6-digit code; three wrong attempts
// disconnect (spec: brute force impractical). Digits are shown as
// typed (requested: visible codes, 30s validity, single-user v1).
func runGatePrompt(sess ssh.Session, gs *auth.GateSession) bool {
	prompt := func(masked string) {
		attempts := 3 - gs.Attempts()
		_, _ = fmt.Fprintf(sess, "\x1b[2J\x1b[HterminalTrade\r\n\r\nEnter TOTP code (6 digits) — %d attempt(s) left:\r\n> %s",
			attempts, masked)
	}
	prompt("")

	buf := make([]byte, 64)
	var code []byte
	for {
		n, err := sess.Read(buf)
		if err != nil {
			return false
		}
		for _, b := range buf[:n] {
			switch {
			case b >= '0' && b <= '9' && len(code) < 6:
				code = append(code, b)
				prompt(string(code)) // visible, per design decision
				if len(code) == 6 {
					ok, allowed := gs.Attempt(string(code))
					if ok {
						return true
					}
					if !allowed {
						_, _ = fmt.Fprint(sess, "\r\nToo many attempts. Disconnecting.\r\n")
						return false
					}
					code = code[:0]
					// brief pause so the failure is visible
					_, _ = fmt.Fprint(sess, "\r\nWrong code.\r\n")
					time.Sleep(700 * time.Millisecond)
					prompt("")
				}
			case b == 3, b == 4: // Ctrl+C / Ctrl+D
				return false
			case b == 127, b == 8: // backspace
				if len(code) > 0 {
					code = code[:len(code)-1]
					prompt(string(code)) // visible
				}
			}
		}
	}
}

// Start begins listening (blocking).
func (s *Server) ListenAndServe() error {
	return s.srv.ListenAndServe()
}

// Close shuts the server down.
func (s *Server) Close() error { return s.srv.Close() }

var _ = log.Info