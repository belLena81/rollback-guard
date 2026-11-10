package domain

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

var sessionNow = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

func testTokenHash(t *testing.T, fill byte) SessionTokenHash {
	t.Helper()
	hash, err := NewSessionTokenHash(bytes.Repeat([]byte{fill}, SessionTokenHashLength))
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func validSessionSnapshot(t *testing.T) SessionSnapshot {
	t.Helper()
	return SessionSnapshot{
		ID:         "session-1",
		UserID:     "user-1",
		TokenHash:  testTokenHash(t, 0xab),
		CreatedAt:  sessionNow,
		ExpiresAt:  sessionNow.Add(8 * time.Hour),
		LastSeenAt: sessionNow,
	}
}

func TestNewSessionTokenHash(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
		ok    bool
	}{
		{name: "32 bytes", input: bytes.Repeat([]byte{1}, 32), ok: true},
		{name: "nil", input: nil},
		{name: "31 bytes", input: bytes.Repeat([]byte{1}, 31)},
		{name: "33 bytes", input: bytes.Repeat([]byte{1}, 33)},
		{name: "all zero", input: make([]byte, 32)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hash, err := NewSessionTokenHash(tt.input)
			if tt.ok {
				if err != nil {
					t.Fatalf("NewSessionTokenHash() error = %v", err)
				}
				if !bytes.Equal(hash.Bytes(), tt.input) {
					t.Fatal("Bytes() does not round-trip the digest")
				}
				return
			}
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("NewSessionTokenHash() error = %v, want ErrValidation", err)
			}
		})
	}
}

func TestSessionTokenHashBytesIsACopy(t *testing.T) {
	hash := testTokenHash(t, 0x11)
	out := hash.Bytes()
	out[0] = 0xff
	if hash.Bytes()[0] != 0x11 {
		t.Fatal("mutating Bytes() changed the hash")
	}
}

func TestSessionTokenHashNeverFormatsItsDigest(t *testing.T) {
	hash := testTokenHash(t, 0xab)
	session, err := RehydrateSession(validSessionSnapshot(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, rendered := range []string{
		fmt.Sprint(hash), fmt.Sprintf("%v", hash), fmt.Sprintf("%+v", hash), fmt.Sprintf("%#v", hash),
		fmt.Sprintf("%s", hash), fmt.Sprintf("%x", hash), fmt.Sprintf("%X", hash), fmt.Sprintf("%q", hash),
		fmt.Sprintf("%v", session), fmt.Sprintf("%+v", session), fmt.Sprintf("%#v", session),
		fmt.Sprintf("%+v", session.Snapshot()),
	} {
		lower := strings.ToLower(rendered)
		if strings.Contains(lower, "abab") || strings.Contains(rendered, "\xab") || strings.Contains(rendered, "171") {
			t.Fatalf("formatted value leaks the digest: %q", rendered)
		}
	}
}

func TestSessionTokenHashEqual(t *testing.T) {
	if !testTokenHash(t, 1).Equal(testTokenHash(t, 1)) {
		t.Fatal("equal digests compare unequal")
	}
	if testTokenHash(t, 1).Equal(testTokenHash(t, 2)) {
		t.Fatal("different digests compare equal")
	}
	if (SessionTokenHash{}).Equal(SessionTokenHash{}) {
		t.Fatal("zero hashes must never compare equal")
	}
}

func TestNewSession(t *testing.T) {
	hash := testTokenHash(t, 7)
	session, err := NewSession("session-1", "user-1", hash, sessionNow, sessionNow.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if session.ID() != "session-1" || session.UserID() != "user-1" || !session.TokenHash().Equal(hash) {
		t.Fatalf("identity fields not kept: %v %v", session.ID(), session.UserID())
	}
	if !session.CreatedAt().Equal(sessionNow) || !session.LastSeenAt().Equal(sessionNow) {
		t.Fatal("created_at and last_seen_at must be now")
	}
	if !session.ExpiresAt().Equal(sessionNow.Add(time.Hour)) {
		t.Fatal("expires_at not kept")
	}
	if session.Revoked() || !session.RevokedAt().IsZero() {
		t.Fatal("a new session is not revoked")
	}
}

func TestRehydrateSessionValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*SessionSnapshot)
		field  string
	}{
		{name: "empty id", mutate: func(s *SessionSnapshot) { s.ID = " " }, field: "session.id"},
		{name: "empty user id", mutate: func(s *SessionSnapshot) { s.UserID = "" }, field: "session.user_id"},
		{name: "zero token hash", mutate: func(s *SessionSnapshot) { s.TokenHash = SessionTokenHash{} }, field: "session.token_hash"},
		{name: "zero created_at", mutate: func(s *SessionSnapshot) { s.CreatedAt = time.Time{} }, field: "session.created_at"},
		{name: "expiry equals creation", mutate: func(s *SessionSnapshot) { s.ExpiresAt = s.CreatedAt }, field: "session.expires_at"},
		{name: "expiry before creation", mutate: func(s *SessionSnapshot) { s.ExpiresAt = s.CreatedAt.Add(-time.Second) }, field: "session.expires_at"},
		{name: "last seen before creation", mutate: func(s *SessionSnapshot) { s.LastSeenAt = s.CreatedAt.Add(-time.Second) }, field: "session.last_seen_at"},
		{name: "zero last seen", mutate: func(s *SessionSnapshot) { s.LastSeenAt = time.Time{} }, field: "session.last_seen_at"},
		{name: "revoked before creation", mutate: func(s *SessionSnapshot) { s.RevokedAt = s.CreatedAt.Add(-time.Second) }, field: "session.revoked_at"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snapshot := validSessionSnapshot(t)
			tt.mutate(&snapshot)
			_, err := RehydrateSession(snapshot)
			var validation *ValidationError
			if !errors.As(err, &validation) || validation.Field != tt.field {
				t.Fatalf("RehydrateSession() error = %v, want validation of %s", err, tt.field)
			}
		})
	}
}

func TestSessionSnapshotRoundTrip(t *testing.T) {
	snapshot := validSessionSnapshot(t)
	snapshot.RevokedAt = sessionNow.Add(time.Minute)
	snapshot.LastSeenAt = sessionNow.Add(30 * time.Second)
	session, err := RehydrateSession(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	got := session.Snapshot()
	if got.ID != snapshot.ID || got.UserID != snapshot.UserID || !got.TokenHash.Equal(snapshot.TokenHash) ||
		!got.CreatedAt.Equal(snapshot.CreatedAt) || !got.ExpiresAt.Equal(snapshot.ExpiresAt) ||
		!got.RevokedAt.Equal(snapshot.RevokedAt) || !got.LastSeenAt.Equal(snapshot.LastSeenAt) {
		t.Fatalf("snapshot round trip differs")
	}
}

func TestSessionIsActive(t *testing.T) {
	expires := sessionNow.Add(time.Hour)
	tests := []struct {
		name      string
		revokedAt time.Time
		now       time.Time
		want      bool
	}{
		{name: "fresh", now: sessionNow, want: true},
		{name: "just before expiry", now: expires.Add(-time.Nanosecond), want: true},
		{name: "at expiry", now: expires, want: false},
		{name: "after expiry", now: expires.Add(time.Second), want: false},
		{name: "revoked", revokedAt: sessionNow.Add(time.Minute), now: sessionNow.Add(2 * time.Minute), want: false},
		{name: "revoked later than now still revoked", revokedAt: sessionNow.Add(time.Minute), now: sessionNow, want: false},
		{name: "zero now", now: time.Time{}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snapshot := validSessionSnapshot(t)
			snapshot.ExpiresAt = expires
			snapshot.RevokedAt = tt.revokedAt
			session, err := RehydrateSession(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if got := session.IsActive(tt.now); got != tt.want {
				t.Fatalf("IsActive(%v) = %v, want %v", tt.now, got, tt.want)
			}
		})
	}
	if (Session{}).IsActive(sessionNow) {
		t.Fatal("the zero Session must never be active")
	}
}

func TestSessionRevoke(t *testing.T) {
	session, err := RehydrateSession(validSessionSnapshot(t))
	if err != nil {
		t.Fatal(err)
	}
	revokeAt := sessionNow.Add(time.Minute)
	revoked, err := session.Revoke(revokeAt)
	if err != nil {
		t.Fatal(err)
	}
	if !revoked.Revoked() || !revoked.RevokedAt().Equal(revokeAt) || revoked.IsActive(revokeAt) {
		t.Fatal("Revoke() did not revoke")
	}
	if session.Revoked() {
		t.Fatal("Revoke() mutated the receiver")
	}

	again, err := revoked.Revoke(revokeAt.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !again.RevokedAt().Equal(revokeAt) {
		t.Fatal("revoking twice must keep the first revocation time")
	}

	for name, now := range map[string]time.Time{"zero": {}, "before creation": sessionNow.Add(-time.Second)} {
		if _, err := session.Revoke(now); !errors.Is(err, ErrValidation) {
			t.Fatalf("Revoke(%s) error = %v, want ErrValidation", name, err)
		}
	}
	if _, err := (Session{}).Revoke(sessionNow); !errors.Is(err, ErrValidation) {
		t.Fatalf("zero Session Revoke() error = %v, want ErrValidation", err)
	}
}

func TestNoActiveSessionErrorCarriesNoDetail(t *testing.T) {
	err := error(&NoActiveSessionError{})
	if !errors.Is(err, ErrNoActiveSession) {
		t.Fatal("NoActiveSessionError must unwrap to ErrNoActiveSession")
	}
	if err.Error() != "no active session" {
		t.Fatalf("Error() = %q", err.Error())
	}
}
