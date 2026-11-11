// Command fake-github runs the scriptable fake GitHub (githubfake) as a
// standalone loopback server for the local end-to-end stack
// (docs/phase7-auth.md, "End-to-end with a real session"). It is a test tool:
// no service image builds it, and it refuses non-loopback addresses.
//
// Environment:
//
//	FAKE_GITHUB_ADDR           listen address, default 127.0.0.1:9180
//	FAKE_GITHUB_CLIENT_ID      required OAuth client ID
//	FAKE_GITHUB_CLIENT_SECRET  required OAuth client secret (never printed)
//	FAKE_GITHUB_USERS          required, "login:id:org+org;login:id:org"
package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/belLena81/rollback-guard/internal/adapters/githuboauth/githubfake"
)

const defaultAddr = "127.0.0.1:9180"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fake-github:", err)
		os.Exit(1)
	}
}

func run() error {
	clientID, secret := os.Getenv("FAKE_GITHUB_CLIENT_ID"), os.Getenv("FAKE_GITHUB_CLIENT_SECRET")
	if clientID == "" || secret == "" {
		return errors.New("FAKE_GITHUB_CLIENT_ID and FAKE_GITHUB_CLIENT_SECRET are required")
	}
	users, err := parseUsers(os.Getenv("FAKE_GITHUB_USERS"))
	if err != nil {
		return err
	}
	addr := os.Getenv("FAKE_GITHUB_ADDR")
	if addr == "" {
		addr = defaultAddr
	}
	fake, err := githubfake.Listen(addr, clientID, secret)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}
	defer fake.Close()
	for _, user := range users {
		fake.AddUser(user)
	}
	fmt.Printf("fake-github: serving %d users on %s\n", len(users), fake.URL())

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	return nil
}

// parseUsers reads "login:id:org+org;login:id" (organizations optional).
func parseUsers(spec string) ([]githubfake.User, error) {
	var users []githubfake.User
	seen := map[string]bool{}
	for entry := range strings.SplitSeq(spec, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.Split(entry, ":")
		if len(parts) < 2 || len(parts) > 3 || parts[0] == "" {
			return nil, errors.New("user entry must be login:id[:org+org]")
		}
		id, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("user %q: id must be a positive integer", parts[0])
		}
		key := strings.ToLower(parts[0])
		if seen[key] {
			return nil, fmt.Errorf("user %q is listed twice", parts[0])
		}
		seen[key] = true
		user := githubfake.User{ID: id, Login: parts[0]}
		if len(parts) == 3 && parts[2] != "" {
			user.Organizations = strings.Split(parts[2], "+")
		}
		users = append(users, user)
	}
	if len(users) == 0 {
		return nil, errors.New("FAKE_GITHUB_USERS is required")
	}
	return users, nil
}
