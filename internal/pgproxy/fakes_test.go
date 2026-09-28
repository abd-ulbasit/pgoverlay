package pgproxy

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgproto3"
)

// pgServer is a fake Postgres server that accepts any number of connections.
// A connection that opens with a CancelRequest has its whole frame reported
// on cancels and is then closed (as Postgres does); any other connection must
// open with a StartupMessage and is handed to session.
type pgServer struct {
	addr    string
	cancels chan []byte
}

func startPGServer(t *testing.T, session func(conn net.Conn, be *pgproto3.Backend)) *pgServer {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lis.Close() })
	srv := &pgServer{addr: lis.Addr().String(), cancels: make(chan []byte, 16)}
	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(10 * time.Second))
				code, payload, err := readStartupFrame(conn)
				if err != nil {
					return
				}
				if code == cancelRequestCode {
					srv.cancels <- append(binary.BigEndian.AppendUint32(nil, uint32(4+len(payload))), payload...)
					return
				}
				var sm pgproto3.StartupMessage
				if err := sm.Decode(payload); err != nil {
					t.Errorf("fake server: decode startup: %v", err)
					return
				}
				session(conn, pgproto3.NewBackend(conn, conn))
			}()
		}
	}()
	return srv
}

// keyedSession returns a fake server session that authenticates with trust,
// hands out BackendKeyData{pid, key}, reports ReadyForQuery and then holds the
// connection until the proxy closes it.
func keyedSession(pid uint32, key []byte) func(net.Conn, *pgproto3.Backend) {
	return func(conn net.Conn, be *pgproto3.Backend) {
		be.Send(&pgproto3.AuthenticationOk{})
		be.Send(&pgproto3.ParameterStatus{Name: "server_version", Value: "17.0"})
		be.Send(&pgproto3.BackendKeyData{ProcessID: pid, SecretKey: key})
		be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		if err := be.Flush(); err != nil {
			return
		}
		io.Copy(io.Discard, conn)
	}
}

// openSession connects through the proxy on conn and reads the startup
// response up to ReadyForQuery, returning the BackendKeyData it carried.
func openSession(t *testing.T, conn net.Conn, branch string) *pgproto3.BackendKeyData {
	t.Helper()
	fe := pgproto3.NewFrontend(conn, conn)
	sendStartup(t, fe, map[string]string{"user": "postgres", "database": "postgres@" + branch})
	var key *pgproto3.BackendKeyData
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("startup response: %v", err)
		}
		switch m := msg.(type) {
		case *pgproto3.BackendKeyData:
			key = &pgproto3.BackendKeyData{ProcessID: m.ProcessID, SecretKey: bytes.Clone(m.SecretKey)}
		case *pgproto3.ReadyForQuery:
			if key == nil {
				t.Fatal("ReadyForQuery before BackendKeyData")
			}
			return key
		case *pgproto3.ErrorResponse:
			t.Fatalf("startup refused: %s", m.Message)
		}
	}
}
