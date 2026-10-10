package notify

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestEmailSendCompletes(t *testing.T) {
	ln := listenLocal(t)
	defer ln.Close()
	body := make(chan string, 1)
	go serveSMTP(ln, "ok", body)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := emailSender{}.Send(ctx, emailRaw(t, ln.Addr().String()), sampleMail())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-body:
		if !strings.Contains(got, "Subject: ") || !strings.Contains(got, "went down") {
			t.Fatalf("message = %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not receive the message")
	}
}

func TestEmailSendStopsWhenServerIsSilent(t *testing.T) {
	ln := listenLocal(t)
	defer ln.Close()
	go serveSMTP(ln, "silent", nil)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := emailSender{}.Send(ctx, emailRaw(t, ln.Addr().String()), sampleMail())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("silent server returned success")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("send took %s, want it bounded by the deadline", elapsed)
	}
}

func TestEmailSendStopsOnCancel(t *testing.T) {
	ln := listenLocal(t)
	defer ln.Close()
	accepted := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		close(accepted)
		_, _ = io.Copy(io.Discard, conn)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- emailSender{}.Send(ctx, emailRaw(t, ln.Addr().String()), sampleMail())
	}()
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("server did not accept")
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("cancelled send returned success")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not stop the send")
	}
}

func listenLocal(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func emailRaw(t *testing.T, addr string) json.RawMessage {
	t.Helper()
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{
		"host": host,
		"port": port,
		"from": "alerts@example.com",
		"to":   "ops@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func sampleMail() Message {
	return Message{
		Event:       EventIncidentStart,
		ServiceName: "api",
		ServiceURL:  "https://example.com",
		StartedAt:   time.Unix(1_700_000_000, 0),
	}
}

func serveSMTP(ln net.Listener, mode string, body chan<- string) {
	conn, err := ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	if mode == "silent" {
		_, _ = io.Copy(io.Discard, conn)
		return
	}
	reader := bufio.NewReader(conn)
	if _, err := conn.Write([]byte("220 localhost ESMTP\r\n")); err != nil {
		return
	}
	var data strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
			_, _ = conn.Write([]byte("250-localhost\r\n250 HELP\r\n"))
		case strings.HasPrefix(upper, "DATA"):
			_, _ = conn.Write([]byte("354 Go ahead\r\n"))
			for {
				row, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				if row == ".\r\n" {
					break
				}
				data.WriteString(row)
			}
			_, _ = conn.Write([]byte("250 OK\r\n"))
		case strings.HasPrefix(upper, "QUIT"):
			_, _ = conn.Write([]byte("221 bye\r\n"))
			if body != nil {
				body <- data.String()
			}
			return
		default:
			_, _ = conn.Write([]byte("250 OK\r\n"))
		}
	}
}
