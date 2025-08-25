package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestListenerEcho(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	l := &Listener{
		Name:    "test",
		Addr:    "127.0.0.1:0",
		MaxConn: 4,
		Logger:  discardLogger(),
		Handler: func(_ context.Context, conn net.Conn) error {
			_, err := conn.Write([]byte("ok\n"))
			return err
		},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- l.ServeListener(ctx, ln) }()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	buf := make([]byte, 4)
	if n, err := conn.Read(buf); err != nil || string(buf[:n]) != "ok\n" {
		t.Fatalf("echo: %q err=%v", buf[:n], err)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after shutdown")
	}
}

func TestListenerRejectsOverLimit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	release := make(chan struct{})
	handler := func(_ context.Context, conn net.Conn) error {
		<-release
		return nil
	}
	l := &Listener{
		Name:    "limit-test",
		Addr:    "127.0.0.1:0",
		MaxConn: 1,
		Logger:  discardLogger(),
		Handler: handler,
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- l.ServeListener(ctx, ln) }()

	first, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	// Give the accept loop time to take the first connection.
	time.Sleep(50 * time.Millisecond)

	second, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	// The second connection must be closed immediately (limit reached).
	_ = second.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, err := second.Read(make([]byte, 1)); err == nil {
		t.Fatalf("expected closed connection, read %d bytes", n)
	}

	close(release)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after shutdown")
	}
}

func TestLimitListenerBackpressure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	lim := NewLimitListener(ln, 1)

	accepted := make(chan net.Conn, 2)
	go func() {
		for i := 0; i < 2; i++ {
			conn, err := lim.Accept()
			if err != nil {
				return
			}
			accepted <- conn
		}
	}()

	first, err := net.Dial("tcp", lim.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	conn1 := <-accepted
	defer conn1.Close()

	// Second connection: the first has not closed, so Accept must block.
	second, err := net.Dial("tcp", lim.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	select {
	case <-accepted:
		t.Fatal("second connection accepted while first is open")
	case <-time.After(150 * time.Millisecond):
	}

	// Closing the first connection releases the slot.
	if err := conn1.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("second connection not accepted after slot release")
	}
}
