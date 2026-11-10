package domain

import "fmt"

// Principal is the authenticated identity behind one request: the live user
// row (including its current role) and the session that authenticated it.
// The Actor that attributes the request in the audit trail is derived from
// the user. It is immutable and holds no token material.
type Principal struct {
	user      User
	sessionID SessionID
	actor     Actor
}

// NewPrincipal binds an enabled user to the session that authenticated it.
func NewPrincipal(user User, sessionID SessionID) (Principal, error) {
	if err := requireText("principal.session_id", string(sessionID)); err != nil {
		return Principal{}, err
	}
	actor, err := ActorFor(user)
	if err != nil {
		return Principal{}, err
	}
	return Principal{user: user, sessionID: sessionID, actor: actor}, nil
}

func (p Principal) User() User           { return p.user }
func (p Principal) SessionID() SessionID { return p.sessionID }
func (p Principal) Actor() Actor         { return p.actor }

// Valid reports whether p was produced by NewPrincipal.
func (p Principal) Valid() bool {
	return p.sessionID != "" && p.actor.Valid() && p.user.Valid()
}

// Format renders only the actor and session IDs.
func (p Principal) Format(f fmt.State, _ rune) {
	_, _ = fmt.Fprintf(f, "Principal{actor:%s session:%s}", p.actor.ID(), p.sessionID) //nolint:errcheck // fmt.Formatter cannot report a write error
}
