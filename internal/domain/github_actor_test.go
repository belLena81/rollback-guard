package domain

import (
	"testing"
	"time"
)

func TestGitHubIDOf(t *testing.T) {
	tests := []struct {
		id     ActorID
		want   int64
		wantOK bool
	}{
		{id: "github:1001", want: 1001, wantOK: true},
		{id: "github:9223372036854775807", want: 9223372036854775807, wantOK: true},
		{id: "github:0"},
		{id: "github:-1"},
		{id: "github:01001"},
		{id: "github:+1001"},
		{id: "github: 1001"},
		{id: "github:"},
		{id: "github:9223372036854775808"},
		{id: "GitHub:1001"},
		{id: "local-admin"},
		{id: "1001"},
	}
	for _, test := range tests {
		t.Run(string(test.id), func(t *testing.T) {
			got, ok := GitHubIDOf(test.id)
			if got != test.want || ok != test.wantOK {
				t.Fatalf("GitHubIDOf(%q) = %d, %v; want %d, %v", test.id, got, ok, test.want, test.wantOK)
			}
		})
	}
}

func TestGitHubIDOfRoundTripsActorFor(t *testing.T) {
	user, err := NewUser("user-1", 4242, "octo", "", nil, RoleAdministrator, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	actor, err := ActorFor(user)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := GitHubIDOf(actor.ID()); !ok || got != 4242 {
		t.Fatalf("GitHubIDOf(ActorFor) = %d, %v", got, ok)
	}
}
