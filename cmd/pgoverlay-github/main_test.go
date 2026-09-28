package main

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Every phase of a request is bounded, not only the headers.
func TestNewServerBoundsEveryPhase(t *testing.T) {
	srv := newServer(":0", http.NotFoundHandler())
	for name, d := range map[string]time.Duration{
		"ReadHeaderTimeout": srv.ReadHeaderTimeout,
		"ReadTimeout":       srv.ReadTimeout,
		"WriteTimeout":      srv.WriteTimeout,
		"IdleTimeout":       srv.IdleTimeout,
	} {
		if d <= 0 || d > 5*time.Minute {
			t.Errorf("%s = %v, want a bound of at most 5m", name, d)
		}
	}
	if srv.ReadTimeout > time.Minute {
		t.Errorf("ReadTimeout = %v: a slow-body client holds a connection that long", srv.ReadTimeout)
	}
	if srv.MaxHeaderBytes <= 0 || srv.MaxHeaderBytes > 1<<20 {
		t.Errorf("MaxHeaderBytes = %d", srv.MaxHeaderBytes)
	}
}

// A client that sends its headers and then stalls in the body is cut off
// once the read deadline passes (shortened here from 30s).
func TestSlowBodyClientIsCutOff(t *testing.T) {
	handlerDone := make(chan struct{})
	srv := newServer("127.0.0.1:0", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		io.ReadAll(r.Body)
	}))
	srv.ReadTimeout = 200 * time.Millisecond
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	defer srv.Close()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "POST /webhook HTTP/1.1\r\nHost: x\r\nContent-Length: 1048576\r\n\r\n{")

	select {
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("handler still reading a stalled body after 5s")
	}
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, _ := bufio.NewReader(conn).ReadString('\n')
	if line != "" && !strings.HasPrefix(line, "HTTP/1.1") {
		t.Errorf("unexpected response %q", line)
	}
}
