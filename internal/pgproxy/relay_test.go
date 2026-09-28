package pgproxy

import (
	"bytes"
	"io"
	"net"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
)

// The startup exchange is relayed byte for byte even though the proxy parses
// it: a client-driven SASL (SCRAM-shaped) exchange crosses the proxy
// untouched in both directions, bytes the backend sends right after
// ReadyForQuery are not lost, and the session keeps working.
func TestStartupRelayIsTransparentThroughSASLExchange(t *testing.T) {
	enc := func(msgs ...pgproto3.Message) []byte {
		var b []byte
		for _, m := range msgs {
			var err error
			if b, err = m.Encode(b); err != nil {
				t.Fatal(err)
			}
		}
		return b
	}
	key := []byte{0x10, 0x20, 0x30, 0x40}
	var (
		saslStart   = enc(&pgproto3.AuthenticationSASL{AuthMechanisms: []string{"SCRAM-SHA-256"}})
		clientFirst = enc(&pgproto3.SASLInitialResponse{AuthMechanism: "SCRAM-SHA-256",
			Data: []byte("n,,n=,r=clientnonce")})
		serverFirst = enc(&pgproto3.AuthenticationSASLContinue{
			Data: []byte("r=clientnonceservernonce,s=c2FsdA==,i=4096")})
		clientFinal = enc(&pgproto3.SASLResponse{Data: []byte("c=biws,r=clientnonceservernonce,p=cHJvb2Y=")})
		// Everything from the final SASL message to one message past
		// ReadyForQuery, sent in a single write.
		serverFinal = enc(
			&pgproto3.AuthenticationSASLFinal{Data: []byte("v=c2lnbmF0dXJl")},
			&pgproto3.AuthenticationOk{},
			&pgproto3.ParameterStatus{Name: "server_version", Value: "17.0"},
			&pgproto3.ParameterStatus{Name: "client_encoding", Value: "UTF8"},
			&pgproto3.BackendKeyData{ProcessID: 555, SecretKey: key},
			&pgproto3.ReadyForQuery{TxStatus: 'I'},
			&pgproto3.NoticeResponse{Severity: "NOTICE", Code: "00000", Message: "right after ReadyForQuery"},
		)
		query    = enc(&pgproto3.Query{String: "SELECT 1"})
		response = enc(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")}, &pgproto3.ReadyForQuery{TxStatus: 'I'})
	)
	readExactly := func(r io.Reader, n int) []byte {
		buf := make([]byte, n)
		if _, err := io.ReadFull(r, buf); err != nil {
			t.Errorf("read %d bytes: %v", n, err)
		}
		return buf
	}

	srv := startPGServer(t, func(conn net.Conn, _ *pgproto3.Backend) {
		conn.Write(saslStart)
		if got := readExactly(conn, len(clientFirst)); !bytes.Equal(got, clientFirst) {
			t.Errorf("backend got SASLInitialResponse %x, want %x", got, clientFirst)
			return
		}
		conn.Write(serverFirst)
		if got := readExactly(conn, len(clientFinal)); !bytes.Equal(got, clientFinal) {
			t.Errorf("backend got SASLResponse %x, want %x", got, clientFinal)
			return
		}
		conn.Write(serverFinal)
		if got := readExactly(conn, len(query)); !bytes.Equal(got, query) {
			t.Errorf("backend got query %x, want %x", got, query)
			return
		}
		conn.Write(response)
		io.Copy(io.Discard, conn)
	})
	addr := startProxy(t, fakeResolver{"pr-1": srv.addr})
	conn := dialProxy(t, addr)
	sendStartup(t, pgproto3.NewFrontend(conn, conn), map[string]string{"user": "alice", "database": "postgres@pr-1"})

	for _, step := range []struct {
		send, want []byte
		name       string
	}{
		{nil, saslStart, "AuthenticationSASL"},
		{clientFirst, serverFirst, "AuthenticationSASLContinue"},
		{clientFinal, serverFinal, "SASL final through ReadyForQuery and the notice after it"},
		{query, response, "query response"},
	} {
		if step.send != nil {
			if _, err := conn.Write(step.send); err != nil {
				t.Fatal(err)
			}
		}
		if got := readExactly(conn, len(step.want)); !bytes.Equal(got, step.want) {
			t.Fatalf("%s: client got %x, want %x", step.name, got, step.want)
		}
	}
}
