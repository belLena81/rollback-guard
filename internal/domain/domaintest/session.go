// Package domaintest builds valid domain values for tests in other packages.
package domaintest

import (
	"bytes"
	"testing"
	"time"

	"github.com/belLena81/rollback-guard/internal/domain"
)

// SessionTokenHash returns a token hash whose bytes are all fill.
func SessionTokenHash(t *testing.T, fill byte) domain.SessionTokenHash {
	t.Helper()
	hash, err := domain.NewSessionTokenHash(bytes.Repeat([]byte{fill}, domain.SessionTokenHashLength))
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

// Session returns a session created at now that expires ttl later.
func Session(t *testing.T, now time.Time, id domain.SessionID, userID domain.UserID, fill byte, ttl time.Duration) domain.Session {
	t.Helper()
	session, err := domain.NewSession(id, userID, SessionTokenHash(t, fill), now, now.Add(ttl))
	if err != nil {
		t.Fatal(err)
	}
	return session
}
