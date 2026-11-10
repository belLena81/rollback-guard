package httpserver

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestLoopbackOnly(t *testing.T) {
	tests := []struct {
		addr net.Addr
		want bool
	}{
		{addr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8080}, want: true},
		{addr: &net.TCPAddr{IP: net.ParseIP("127.0.0.53"), Port: 8080}, want: true},
		{addr: &net.TCPAddr{IP: net.ParseIP("::1"), Port: 8080}, want: true},
		{addr: &net.TCPAddr{IP: net.ParseIP("0.0.0.0"), Port: 8080}, want: false},
		{addr: &net.TCPAddr{IP: net.ParseIP("::"), Port: 8080}, want: false},
		{addr: &net.TCPAddr{IP: net.ParseIP("10.0.0.7"), Port: 8080}, want: false},
		{addr: &net.TCPAddr{Port: 8080}, want: false},
		{addr: &net.UnixAddr{Name: "/tmp/socket", Net: "unix"}, want: false},
	}
	for _, test := range tests {
		if got := loopbackOnly(test.addr); got != test.want {
			t.Errorf("loopbackOnly(%s) = %t, want %t", test.addr, got, test.want)
		}
	}
}

func TestServeWarnsOnlyWhenListeningBeyondLoopback(t *testing.T) {
	for _, test := range []struct {
		name, addr string
		wantWarn   bool
	}{
		{name: "loopback", addr: "127.0.0.1:0"},
		{name: "all interfaces", addr: "0.0.0.0:0", wantWarn: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", test.addr)
			if err != nil {
				t.Fatal(err)
			}
			_, port, err := net.SplitHostPort(listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- serve(ctx, listener, "svc", http.NotFoundHandler(), slog.New(slog.NewTextHandler(&logs, nil)), defaultTimeouts())
			}()
			// One answered request proves the start-up lines were logged.
			if _, err := fetch(t, http.MethodGet, "http://"+net.JoinHostPort("127.0.0.1", port)); err != nil {
				t.Fatal(err)
			}
			cancel()
			if err := <-done; err != nil {
				t.Fatal(err)
			}

			warned := strings.Contains(logs.String(), "level=WARN") && strings.Contains(logs.String(), "no authentication")
			if warned != test.wantWarn {
				t.Fatalf("warned = %t, want %t; logs:\n%s", warned, test.wantWarn, logs.String())
			}
		})
	}
}

// rawRequest opens a connection, sends the given bytes, and returns what the
// server does within the client deadline: the bytes read and whether the
// client's own deadline expired first.
func rawRequest(t *testing.T, url, payload string, clientDeadline time.Duration) (response string, clientTimedOut bool) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(url, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close connection: %v", err)
		}
	}()
	if payload != "" {
		if _, err := io.WriteString(conn, payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := conn.SetReadDeadline(time.Now().Add(clientDeadline)); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(bufio.NewReader(conn))
	var netErr net.Error
	return string(data), errors.As(err, &netErr) && netErr.Timeout()
}

// Each read-side timeout is, in its case, the only one short enough to end
// the stalled connection, so removing it from http.Server fails its case.
func TestReadSideTimeoutsDropStalledConnections(t *testing.T) {
	const long = time.Minute
	tests := []struct {
		name    string
		limits  timeouts
		payload string
	}{
		{name: "headers never finish (ReadHeaderTimeout)", limits: timeouts{readHeader: 100 * time.Millisecond, read: long, idle: long, write: long, shutdown: time.Second}, payload: "GET / HTTP/1.1\r\nHost: stalled\r\n"},
		{name: "body never finishes (ReadTimeout)", limits: timeouts{readHeader: long, read: 100 * time.Millisecond, idle: long, write: long, shutdown: time.Second}, payload: "POST /echo HTTP/1.1\r\nHost: stalled\r\nContent-Length: 10\r\n\r\nabc"},
	}
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			http.Error(w, "incomplete body", http.StatusBadRequest)
			return
		}
		writeBody(w, "ok")
	})
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			url, _ := startServer(t, echo, test.limits)
			if _, timedOut := rawRequest(t, url, test.payload, 5*time.Second); timedOut {
				t.Fatal("server kept the stalled connection open")
			}
		})
	}
}

func TestIdleTimeoutClosesKeepAliveConnections(t *testing.T) {
	const long = time.Minute
	limits := timeouts{readHeader: long, read: long, idle: 100 * time.Millisecond, write: long, shutdown: time.Second}
	url, _ := startServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeBody(w, "ok") }), limits)

	// The request completes; the connection then stays open until the idle
	// timeout closes it, which ends the read.
	response, timedOut := rawRequest(t, url, "GET / HTTP/1.1\r\nHost: idle\r\n\r\n", 5*time.Second)

	if timedOut {
		t.Fatal("server kept an idle keep-alive connection open past IdleTimeout")
	}
	if !strings.HasPrefix(response, "HTTP/1.1 200") {
		t.Fatalf("response = %q", response)
	}
}
