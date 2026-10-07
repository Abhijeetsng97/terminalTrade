// Package auth implements the TOTP gate over SSH pubkey auth.
package auth

import (
	"fmt"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
)

// Gate checks TOTP codes. First run generates and persists a secret.
type Gate struct {
	secret string
	// window: how many periods off we accept (1 = one period skew)
	skew uint
	// for tests
	now func() time.Time
}

func NewGate(secret string) *Gate {
	return &Gate{secret: secret, skew: 1}
}

// EnsureSecret returns the secret, generating one when empty (first
// run). The caller persists it.
func (g *Gate) EnsureSecret() (string, bool, error) {
	if g.secret != "" {
		return g.secret, false, nil
	}
	key, err := totp.Generate(totp.GenerateOpts{
		Issuer:      "terminalTrade",
		AccountName: "trader",
		Period:      30,
		Digits:      otp.DigitsSix,
	})
	if err != nil {
		return "", false, err
	}
	g.secret = key.Secret()
	return g.secret, true, nil
}

// Secret exposes the current secret for persistence.
func (g *Gate) Secret() string { return g.secret }

// URL renders the otpauth enrollment URL (first run only).
func (g *Gate) URL() string {
	key, err := otp.NewKeyFromURL("otpauth://totp/terminalTrade:trader?secret=" + g.secret)
	if err != nil {
		return ""
	}
	return key.URL()
}

// Verify checks a 6-digit code with skew 1 (±30s).
func (g *Gate) Verify(code string) bool {
	if len(code) != 6 {
		return false
	}
	now := g.nowOrReal()
	ok, err := totp.ValidateCustom(code, g.secret, now, totp.ValidateOpts{
		Period: 30, Skew: g.skew, Digits: otp.DigitsSix,
	})
	return err == nil && ok
}

func (g *Gate) nowOrReal() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

// GateSession tracks attempts per SSH connection.
type GateSession struct {
	gate     *Gate
	attempts int
	// Allowed after success
	done bool
}

func NewGateSession(g *Gate) *GateSession {
	return &GateSession{gate: g}
}

// Attempt verifies and counts; returns allowed=false when the
// connection should be dropped (3 strikes).
func (gs *GateSession) Attempt(code string) (ok bool, allowed bool) {
	if gs.done {
		return true, true
	}
	if gs.gate.Verify(code) {
		gs.done = true
		return true, true
	}
	gs.attempts++
	if gs.attempts >= 3 {
		return false, false
	}
	return false, true
}

// Attempts reports the strike count.
func (gs *GateSession) Attempts() int { return gs.attempts }

// EnrollInfo is the first-run payload.
type EnrollInfo struct {
	Secret string
	URL    string
}

func (g *Gate) Enroll() (EnrollInfo, error) {
	secret, fresh, err := g.EnsureSecret()
	if err != nil {
		return EnrollInfo{}, err
	}
	_ = fresh
	url := g.URL()
	if url == "" {
		return EnrollInfo{}, fmt.Errorf("cannot render TOTP url")
	}
	return EnrollInfo{Secret: secret, URL: url}, nil
}