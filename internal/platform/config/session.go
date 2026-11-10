package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Sessions (docs/phase7-auth.md, "Sessions"). The TTL bounds keep a session
// long enough to finish an incident and short enough that a stolen token
// dies within a month even if nobody revokes it.
const (
	defaultSessionTTL          = 8 * time.Hour
	MinSessionTTL              = 5 * time.Minute
	MaxSessionTTL              = 30 * 24 * time.Hour
	defaultSessionCookieSecure = true
)

// SessionConfig configures server-side sessions of the orchestrator.
type SessionConfig struct {
	// TTL is the lifetime of a session from issue (SESSION_TTL).
	TTL time.Duration
	// CookieSecure sets the Secure attribute of the session cookie
	// (SESSION_COOKIE_SECURE). Only plain-HTTP local development turns it off.
	CookieSecure bool
}

func sessionEnv() (SessionConfig, error) {
	ttl, err := boundedDurationEnv("SESSION_TTL", defaultSessionTTL, MinSessionTTL, MaxSessionTTL)
	if err != nil {
		return SessionConfig{}, err
	}
	secure, err := boolEnv("SESSION_COOKIE_SECURE", defaultSessionCookieSecure)
	if err != nil {
		return SessionConfig{}, err
	}
	return SessionConfig{TTL: ttl, CookieSecure: secure}, nil
}

// boolEnv reads true or false (strconv.ParseBool spellings).
func boolEnv(name string, fallback bool) (bool, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false: %w", name, err)
	}
	return parsed, nil
}
