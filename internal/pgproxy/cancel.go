package pgproxy

import (
	"encoding/binary"
	"log/slog"
	"net"
	"sync"
	"time"
)

// cancelMap records, for every live session, the backend that issued its
// cancel key. The key is the BackendKeyData body (process ID + secret key:
// 4 key bytes under protocol 3.0, up to 256 under 3.2); a CancelRequest
// carries the same bytes after its request code. Entries are added when the
// backend sends BackendKeyData and removed when the session ends.
//
// Branches are separate Postgres servers, so two of them can hand out the same
// key. A key held by more than one live session is ambiguous and is not
// forwarded: cancelling another client's query is worse than not cancelling.
// The zero value is ready to use.
type cancelMap struct {
	mu sync.Mutex
	m  map[string][]string // key -> backend addrs of the live sessions holding it
}

func (c *cancelMap) add(key, addr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = make(map[string][]string)
	}
	c.m[key] = append(c.m[key], addr)
}

func (c *cancelMap) remove(key, addr string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	addrs := c.m[key]
	for i, a := range addrs {
		if a == addr {
			addrs = append(addrs[:i:i], addrs[i+1:]...)
			break
		}
	}
	if len(addrs) == 0 {
		delete(c.m, key)
	} else {
		c.m[key] = addrs
	}
}

// lookup returns the backend address for key when exactly one live session
// holds it.
func (c *cancelMap) lookup(key string) (addr string, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if addrs := c.m[key]; len(addrs) == 1 {
		return addrs[0], true
	}
	return "", false
}

func (c *cancelMap) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

// forwardCancel sends a CancelRequest to the backend of the live session
// whose key it carries. payload is the frame after its length word: the
// request code, the process ID and the secret key. Unknown and ambiguous keys
// are dropped silently, as Postgres does for keys it does not know.
//
// Like libpq, it waits for the backend to close the cancel connection before
// returning (and so before the caller closes the client's): that is the
// signal that the cancel has been processed, and clients rely on it so that a
// query they send after the cancel returns is not the one cancelled.
func (p *Proxy) forwardCancel(payload []byte) {
	if len(payload) < 12 { // code + process ID + at least a 4-byte key
		return
	}
	addr, ok := p.cancels.lookup(string(payload[4:]))
	if !ok {
		return
	}
	backend, err := net.DialTimeout("tcp", addr, p.dialTimeout())
	if err != nil {
		slog.Warn("pgproxy: cancel request not forwarded", "addr", addr, "error", err)
		return
	}
	defer backend.Close()
	backend.SetDeadline(time.Now().Add(p.dialTimeout()))
	frame := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(frame, uint32(len(frame)))
	copy(frame[4:], payload)
	if _, err := backend.Write(frame); err != nil {
		slog.Warn("pgproxy: cancel request not forwarded", "addr", addr, "error", err)
		return
	}
	var b [1]byte
	backend.Read(b[:]) // returns on the backend's close (or the deadline)
}
