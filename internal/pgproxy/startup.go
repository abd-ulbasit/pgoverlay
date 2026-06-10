package pgproxy

import (
	"encoding/binary"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5/pgproto3"
)

// Startup-phase request codes (the 4 bytes after the length prefix).
const (
	cancelRequestCode = 80877102
	sslRequestCode    = 80877103
	gssEncRequestCode = 80877104
)

// Postgres bounds for startup packets (matching pgproto3's backend).
const (
	minStartupPacketLen = 4     // just the request/version code
	maxStartupPacketLen = 10000 // sanity limit, same as the PG server
)

// readStartupFrame reads exactly one startup-phase packet: a 4-byte big-endian
// length (which includes itself) followed by the payload. It never reads past
// the frame, so the connection can be handed to a raw byte relay afterwards.
// The returned payload starts with the 4-byte request/version code.
func readStartupFrame(r io.Reader) (code uint32, payload []byte, err error) {
	var head [4]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return 0, nil, err
	}
	size := int(int32(binary.BigEndian.Uint32(head[:]))) - 4
	if size < minStartupPacketLen || size > maxStartupPacketLen {
		return 0, nil, fmt.Errorf("invalid startup packet length %d", size+4)
	}
	payload = make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return binary.BigEndian.Uint32(payload[:4]), payload, nil
}

// splitDatabase splits a routed database name on its LAST '@' so database
// names containing '@' still work: "app@db@pr-1" -> ("app@db", "pr-1").
// ok is false when there is no '@' at all.
func splitDatabase(db string) (dbname, branch string, ok bool) {
	i := strings.LastIndexByte(db, '@')
	if i < 0 {
		return "", "", false
	}
	return db[:i], db[i+1:], true
}

// writeRefusal sends a Postgres-style FATAL ErrorResponse and leaves closing
// the connection to the caller. code is a SQLSTATE, e.g. 3D000.
func writeRefusal(w io.Writer, code, message string) error {
	er := &pgproto3.ErrorResponse{
		Severity:            "FATAL",
		SeverityUnlocalized: "FATAL",
		Code:                code,
		Message:             message,
	}
	buf, err := er.Encode(nil)
	if err != nil {
		return err
	}
	_, err = w.Write(buf)
	return err
}
