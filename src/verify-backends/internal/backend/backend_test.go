package backend_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/cloudfoundry/juicefs-release/src/verify-backends/internal/backend"
	"github.com/jackc/pgx/v5/pgproto3"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestPostgres(t *testing.T) { RegisterFailHandler(Fail); RunSpecs(t, "PostgreSQL Adapter Suite") }

// Exercise the real pgx client over TCP without requiring a PostgreSQL daemon.
// The server accepts only the startup and SELECT 1 exchange used by the verifier.
var _ = Describe("PostgreSQL health check", func() {
	var listener net.Listener
	var workers sync.WaitGroup
	var serverErrors chan error
	BeforeEach(func() {
		var err error
		listener, err = net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		serverErrors = make(chan error, 1)
	})
	AfterEach(func() { listener.Close(); workers.Wait() })
	serve := func(value string, queryFailure bool) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			conn, err := listener.Accept()
			if err != nil {
				serverErrors <- err
				return
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			protocol := pgproto3.NewBackend(conn, conn)
			if _, err = protocol.ReceiveStartupMessage(); err != nil {
				serverErrors <- err
				return
			}
			protocol.Send(&pgproto3.AuthenticationOk{})
			protocol.Send(&pgproto3.ParameterStatus{Name: "server_version", Value: "16.0"})
			protocol.Send(&pgproto3.ParameterStatus{Name: "standard_conforming_strings", Value: "on"})
			protocol.Send(&pgproto3.ParameterStatus{Name: "client_encoding", Value: "UTF8"})
			protocol.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
			if err = protocol.Flush(); err != nil {
				serverErrors <- err
				return
			}
			message, err := protocol.Receive()
			if err != nil {
				serverErrors <- err
				return
			}
			query, ok := message.(*pgproto3.Query)
			if !ok || query.String != "SELECT 1" {
				serverErrors <- fmt.Errorf("unexpected query: %v", message)
				return
			}
			if queryFailure {
				protocol.Send(&pgproto3.ErrorResponse{Severity: "ERROR", Code: "42501", Message: "permission denied"})
			} else {
				protocol.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{{Name: []byte("?column?"), DataTypeOID: 23, DataTypeSize: 4, TypeModifier: -1}}})
				protocol.Send(&pgproto3.DataRow{Values: [][]byte{[]byte(value)}})
				protocol.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
			}
			protocol.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
			if err = protocol.Flush(); err != nil {
				serverErrors <- err
				return
			}
			serverErrors <- nil
		}()
	}
	check := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return (backend.Postgres{}).Check(ctx, "postgres://user@"+listener.Addr().String()+"/juicefs?sslmode=disable&default_query_exec_mode=simple_protocol")
	}
	It("connects and accepts exactly the expected query result", func() { serve("1", false); Expect(check()).To(Succeed()); Expect(<-serverErrors).NotTo(HaveOccurred()) })
	It("rejects unexpected query results", func() {
		serve("2", false)
		Expect(check()).To(MatchError("postgres SELECT 1 returned 2"))
		Expect(<-serverErrors).NotTo(HaveOccurred())
	})
	It("propagates query errors", func() {
		serve("", true)
		Expect(check()).To(MatchError(ContainSubstring("permission denied")))
		Expect(<-serverErrors).NotTo(HaveOccurred())
	})
	It("respects canceled connection contexts", func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := (backend.Postgres{}).Check(ctx, "postgres://user@"+listener.Addr().String()+"/juicefs?sslmode=disable")
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, context.Canceled)).To(BeTrue())
	})
})
