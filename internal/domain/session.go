package domain

import (
	"crypto/subtle"
	"fmt"
	"time"
)

// SessionID is the orchestrator's internal identifier of a session record.
type SessionID string

// SessionTokenHashLength is the size of a SHA-256 digest.
const SessionTokenHashLength = 32

// SessionTokenHash is the SHA-256 digest of an opaque session token. The raw
// token is never part of the domain (docs/adr-auth-sessions.md). The digest
// itself is credential-derived, so every fmt verb renders it as a redaction
// marker; only Bytes exposes it, for persistence.
type SessionTokenHash struct {
	digest [SessionTokenHashLength]byte
}

// NewSessionTokenHash wraps a 32-byte SHA-256 digest. An all-zero digest is
// rejected: it is the zero value, not the hash of any issued token.
func NewSessionTokenHash(digest []byte) (SessionTokenHash, error) {
	if len(digest) != SessionTokenHashLength {
		return SessionTokenHash{}, invalid("session.token_hash", fmt.Sprintf("must be %d bytes", SessionTokenHashLength))
	}
	var hash SessionTokenHash
	copy(hash.digest[:], digest)
	if hash.IsZero() {
		return SessionTokenHash{}, invalid("session.token_hash", "must not be all zero")
	}
	return hash, nil
}

// IsZero reports whether h is the unset zero value.
func (h SessionTokenHash) IsZero() bool {
	return h.digest == [SessionTokenHashLength]byte{}
}

// Bytes returns a copy of the digest for storage.
func (h SessionTokenHash) Bytes() []byte {
	return append([]byte(nil), h.digest[:]...)
}

// Equal compares two digests in constant time. Zero values never match.
func (h SessionTokenHash) Equal(other SessionTokenHash) bool {
	if h.IsZero() || other.IsZero() {
		return false
	}
	return subtle.ConstantTimeCompare(h.digest[:], other.digest[:]) == 1
}

const redacted = "[redacted]"

// Format renders a redaction marker for every verb, so a digest never reaches
// a log line or error message by accident.
func (SessionTokenHash) Format(f fmt.State, _ rune) {
	_, _ = f.Write([]byte(redacted)) //nolint:errcheck // fmt.Formatter cannot report a write error
}

// Session is a server-side login session. It is immutable: Revoke returns a
// new value.
type Session struct {
	id         SessionID
	userID     UserID
	tokenHash  SessionTokenHash
	createdAt  time.Time
	expiresAt  time.Time
	revokedAt  time.Time
	lastSeenAt time.Time
}

// SessionSnapshot is the persisted form of a Session. A zero RevokedAt means
// the session has not been revoked.
type SessionSnapshot struct {
	ID         SessionID
	UserID     UserID
	TokenHash  SessionTokenHash
	CreatedAt  time.Time
	ExpiresAt  time.Time
	RevokedAt  time.Time
	LastSeenAt time.Time
}

// NewSession builds an unrevoked session created and last seen at now.
func NewSession(id SessionID, userID UserID, tokenHash SessionTokenHash, now, expiresAt time.Time) (Session, error) {
	return RehydrateSession(SessionSnapshot{
		ID:         id,
		UserID:     userID,
		TokenHash:  tokenHash,
		CreatedAt:  now,
		ExpiresAt:  expiresAt,
		LastSeenAt: now,
	})
}

// RehydrateSession rebuilds a Session from storage, applying every
// constructor rule.
func RehydrateSession(s SessionSnapshot) (Session, error) {
	if err := requireText("session.id", string(s.ID)); err != nil {
		return Session{}, err
	}
	if err := requireText("session.user_id", string(s.UserID)); err != nil {
		return Session{}, err
	}
	if s.TokenHash.IsZero() {
		return Session{}, invalid("session.token_hash", "must be set")
	}
	if s.CreatedAt.IsZero() {
		return Session{}, invalid("session.created_at", "must be set")
	}
	if !s.ExpiresAt.After(s.CreatedAt) {
		return Session{}, invalid("session.expires_at", "must be after created_at")
	}
	if s.LastSeenAt.IsZero() || s.LastSeenAt.Before(s.CreatedAt) {
		return Session{}, invalid("session.last_seen_at", "must be set and not before created_at")
	}
	if !s.RevokedAt.IsZero() && s.RevokedAt.Before(s.CreatedAt) {
		return Session{}, invalid("session.revoked_at", "must not be before created_at")
	}
	session := Session{
		id:         s.ID,
		userID:     s.UserID,
		tokenHash:  s.TokenHash,
		createdAt:  s.CreatedAt.UTC(),
		expiresAt:  s.ExpiresAt.UTC(),
		lastSeenAt: s.LastSeenAt.UTC(),
	}
	if !s.RevokedAt.IsZero() {
		session.revokedAt = s.RevokedAt.UTC()
	}
	return session, nil
}

func (s Session) ID() SessionID               { return s.id }
func (s Session) UserID() UserID              { return s.userID }
func (s Session) TokenHash() SessionTokenHash { return s.tokenHash }
func (s Session) CreatedAt() time.Time        { return s.createdAt }
func (s Session) ExpiresAt() time.Time        { return s.expiresAt }
func (s Session) RevokedAt() time.Time        { return s.revokedAt }
func (s Session) LastSeenAt() time.Time       { return s.lastSeenAt }
func (s Session) Revoked() bool               { return !s.revokedAt.IsZero() }

// Valid reports whether s was produced by NewSession or RehydrateSession.
func (s Session) Valid() bool {
	return s.id != "" && s.userID != "" && !s.tokenHash.IsZero() && s.expiresAt.After(s.createdAt)
}

// Snapshot returns the persisted form of s.
func (s Session) Snapshot() SessionSnapshot {
	return SessionSnapshot{
		ID:         s.id,
		UserID:     s.userID,
		TokenHash:  s.tokenHash,
		CreatedAt:  s.createdAt,
		ExpiresAt:  s.expiresAt,
		RevokedAt:  s.revokedAt,
		LastSeenAt: s.lastSeenAt,
	}
}

// IsActive reports whether s may authenticate a request at now: it is not
// revoked and now is before its expiry.
func (s Session) IsActive(now time.Time) bool {
	return s.Valid() && !s.Revoked() && !now.IsZero() && now.Before(s.expiresAt)
}

// Revoke returns a copy of s revoked at now. Revoking a revoked session keeps
// the first revocation time.
func (s Session) Revoke(now time.Time) (Session, error) {
	if !s.Valid() {
		return Session{}, invalid("session", "must be a constructed session")
	}
	if now.IsZero() || now.Before(s.createdAt) {
		return Session{}, invalid("session.revoked_at", "must be set and not before created_at")
	}
	if s.Revoked() {
		return s, nil
	}
	next := s
	next.revokedAt = now.UTC()
	return next, nil
}

// Format renders only the session and user IDs, never the token hash.
func (s Session) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "Session{id:%s user:%s}", s.id, s.userID) //nolint:errcheck // fmt.Formatter cannot report a write error
}
