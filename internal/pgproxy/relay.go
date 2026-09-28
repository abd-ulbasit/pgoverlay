package pgproxy

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// maxBackendKeyDataLen bounds the BackendKeyData body the proxy records: a
// 4-byte process ID plus a secret key of at most 256 bytes (protocol 3.2; it
// is exactly 4 bytes under 3.0).
const maxBackendKeyDataLen = 4 + 256

// session is one routed client<->backend connection pair.
type session struct {
	client    net.Conn // what the client speaks: the TLS conn when TLS is in use
	clientRaw net.Conn // the client's TCP conn, closed to force a teardown
	backend   net.Conn
	addr      string // backend address, recorded against the cancel key
	idle      time.Duration
	grace     time.Duration
	cancels   *cancelMap
	onReady   func() // runs once, when the backend first sends ReadyForQuery

	ready atomic.Bool
	// cancelKey is the BackendKeyData body registered in cancels. Written only
	// by the backend->client goroutine; read by relay() after both finish.
	cancelKey string
}

// relay copies bytes in both directions until both are done.
//
// Until the backend's first ReadyForQuery the connections keep the deadline
// route() set (AuthTimeout), so authentication is bounded by the proxy. From
// then on every read in either direction bumps both connections' read AND
// write deadlines forward by idle, so a session with no bytes flowing either
// way, or one stuck writing to a peer that stopped reading, times out.
// (Bumping both sides, not just the active one, means a busy direction keeps
// the quiet direction alive, so only truly idle sessions are closed, never
// merely one-directional ones.)
//
// Each direction propagates EOF with a half-close (CloseWrite) so data still
// in flight the other way can drain. Once either direction has ended the other
// gets grace to finish, then both connections are closed: nothing useful can
// follow a backend close, and a client that has gone quiet must not keep its
// connection slot until the idle timeout.
func (s *session) relay() {
	var (
		wg    sync.WaitGroup
		once  sync.Once
		timer *time.Timer
	)
	ended := func() {
		once.Do(func() {
			timer = time.AfterFunc(s.grace, func() {
				s.clientRaw.Close()
				s.backend.Close()
			})
		})
	}
	wg.Add(2)
	go func() {
		defer wg.Done()
		s.copyClientToBackend()
		ended()
	}()
	go func() {
		defer wg.Done()
		s.copyBackendToClient()
		ended()
	}()
	wg.Wait()
	timer.Stop()
	if s.cancelKey != "" {
		s.cancels.remove(s.cancelKey, s.addr)
	}
}

func (s *session) copyClientToBackend() {
	io.Copy(s.backend, &idleReader{s: s, src: s.client})
	closeWrite(s.backend)
}

func (s *session) copyBackendToClient() {
	defer closeWrite(s.client)
	br := bufio.NewReader(s.backend)
	if err := s.relayStartup(br); err != nil {
		return
	}
	// Anything the backend sent right after ReadyForQuery is already in br;
	// hand it over before switching to the plain copy.
	if n := br.Buffered(); n > 0 {
		buf, _ := br.Peek(n)
		if _, err := s.client.Write(buf); err != nil {
			return
		}
	}
	io.Copy(s.client, &idleReader{s: s, src: s.backend})
}

// relayStartup copies the backend's startup response to the client message by
// message until the first ReadyForQuery, which it also copies. The bytes are
// forwarded unchanged; the proxy only reads the frame headers (type byte and
// length) and the BackendKeyData body, whose key it records so a later
// CancelRequest can be routed here. Authentication messages are streamed
// through without being inspected, and everything the client sends meanwhile
// (a SCRAM exchange, say) is relayed by the other direction untouched.
//
// It returns nil after ReadyForQuery (having marked the session ready), or the
// error that ended the exchange (the backend closing after an ErrorResponse, a
// deadline, ...). Everything read from the backend is flushed to the client in
// either case.
func (s *session) relayStartup(src *bufio.Reader) error {
	w := bufio.NewWriter(s.client)
	defer w.Flush() // error paths: deliver what was read (e.g. a FATAL ErrorResponse)
	var hdr [5]byte
	for {
		// About to wait on the backend: push what we have to the client first.
		if src.Buffered() == 0 {
			if err := w.Flush(); err != nil {
				return err
			}
		}
		if _, err := io.ReadFull(src, hdr[:]); err != nil {
			return err
		}
		n := int64(int32(binary.BigEndian.Uint32(hdr[1:]))) - 4
		if n < 0 {
			return fmt.Errorf("pgproxy: backend sent message %q with invalid length %d", hdr[0], n+4)
		}
		if _, err := w.Write(hdr[:]); err != nil {
			return err
		}
		if hdr[0] == 'K' && n >= 8 && n <= maxBackendKeyDataLen {
			body := make([]byte, n)
			if _, err := io.ReadFull(src, body); err != nil {
				return err
			}
			if _, err := w.Write(body); err != nil {
				return err
			}
			s.registerCancelKey(body)
		} else if _, err := io.CopyN(w, src, n); err != nil {
			return err
		}
		if hdr[0] == 'Z' {
			// Authenticated. Switch to idle deadlines and release the startup
			// slot before the client can see ReadyForQuery and act on it.
			s.markReady()
			return w.Flush()
		}
	}
}

// registerCancelKey records body (process ID + secret key) as this session's
// cancel key, replacing any earlier one.
func (s *session) registerCancelKey(body []byte) {
	if s.cancelKey != "" {
		s.cancels.remove(s.cancelKey, s.addr)
	}
	s.cancelKey = string(body)
	s.cancels.add(s.cancelKey, s.addr)
}

// markReady ends the startup phase: switch both connections from the
// AuthTimeout deadline to idle deadlines and release the client IP's startup
// slot.
func (s *session) markReady() {
	s.ready.Store(true)
	s.bump()
	if s.onReady != nil {
		s.onReady()
	}
}

// bump pushes both connections' read and write deadlines idle into the
// future.
func (s *session) bump() {
	d := time.Now().Add(s.idle)
	s.client.SetDeadline(d)
	s.backend.SetDeadline(d)
}

// idleReader bumps both connections' deadlines on every successful read once
// the session is authenticated, so any activity in either direction keeps the
// whole session alive and a fully quiet or stuck session times out after
// idle. Before that, the AuthTimeout deadline stands.
type idleReader struct {
	s   *session
	src net.Conn
}

func (r *idleReader) Read(b []byte) (int, error) {
	n, err := r.src.Read(b)
	if n > 0 && r.s.ready.Load() {
		r.s.bump()
	}
	return n, err
}

// closeWrite half-closes c when it supports that (TCP, TLS), else closes it.
func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	} else {
		c.Close()
	}
}
