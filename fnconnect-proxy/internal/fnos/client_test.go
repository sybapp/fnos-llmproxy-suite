package fnos

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"testing"
	"time"
)

func TestLongTokenRefreshRetainsSigningKey(t *testing.T) {
	local, remote := net.Pipe()
	defer local.Close()
	defer remote.Close()
	key := base64.StdEncoding.EncodeToString([]byte("established signing secret"))
	client := New("unused", time.Second)
	client.ws = &wsConn{conn: local, br: bufio.NewReader(local)}
	client.aesKey = "01234567890123456789012345678901"
	client.iv = make([]byte, 16)
	done := make(chan error, 1)
	go func() {
		server := &wsConn{conn: remote, br: bufio.NewReader(remote)}
		for _, expected := range []string{"util.crypto.getRSAPub", "user.tokenLogin", "appcgi.sac.entry.v1.exchangeEntryToken"} {
			b, err := server.ReadMessage()
			if err != nil {
				done <- err
				return
			}
			raw := b
			if expected != "util.crypto.getRSAPub" {
				if len(b) < 44 {
					done <- errors.New("unsigned request")
					return
				}
				raw = b[44:]
				sig, _ := hmacSignature(raw, key)
				if string(b[:44]) != sig {
					done <- errors.New("wrong signing key after tokenLogin")
					return
				}
			}
			var request map[string]any
			if err := json.Unmarshal(raw, &request); err != nil {
				done <- err
				return
			}
			if fieldString(request, "req") != expected {
				done <- errors.New("unexpected request")
				return
			}
			response := map[string]any{"reqid": request["reqid"]}
			switch expected {
			case "util.crypto.getRSAPub":
				response["pub"] = "unused-for-signed-refresh"
				response["si"] = "test-si"
			case "user.tokenLogin":
				if fieldString(request, "token") != "long" {
					done <- errors.New("wrong long token")
					return
				}
				response["token"] = "renewed-fnos"
				response["backId"] = "new-back"
				response["secret"] = "opaque-secret-not-encrypted-with-this-connection-key"
			default:
				response["data"] = map[string]any{"token": "renewed-entry"}
			}
			b, _ = json.Marshal(response)
			if err := server.WriteText(string(b)); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	sess, err := client.RefreshWithLongToken(context.Background(), "long", key, "old-back", "Browser", "test")
	if err != nil {
		t.Fatal(err)
	}
	if sess.FNOSToken != "renewed-fnos" || sess.EntryToken != "renewed-entry" || sess.SignKey != key || sess.BackID != "new-back" {
		t.Fatalf("unexpected session: %+v", sess)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCancelledRequestClosesReader(t *testing.T) {
	for _, cancelRequest := range []bool{true, false} {
		t.Run(map[bool]string{true: "cancel", false: "timeout"}[cancelRequest], func(t *testing.T) {
			local, remote := net.Pipe()
			defer remote.Close()
			client := New("unused", 50*time.Millisecond)
			defer client.Close()
			client.ws = &wsConn{conn: local, br: bufio.NewReader(local)}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ended := make(chan error, 1)
			go func() {
				server := &wsConn{conn: remote, br: bufio.NewReader(remote)}
				if _, err := server.ReadMessage(); err != nil {
					ended <- err
					return
				}
				if cancelRequest {
					cancel()
				}
				_, err := server.ReadMessage()
				ended <- err
			}()
			if _, err := client.request(ctx, "ping", nil, false); err == nil {
				t.Fatal("request succeeded without response")
			}
			if cancelRequest && !errors.Is(ctx.Err(), context.Canceled) {
				t.Fatal("context was not cancelled")
			}
			select {
			case err := <-ended:
				if err == nil {
					t.Fatal("socket remained readable")
				}
			case <-time.After(time.Second):
				t.Fatal("reader/socket leaked")
			}
			started := time.Now()
			if _, err := client.request(context.Background(), "ping", nil, false); err == nil {
				t.Fatal("reused closed socket")
			}
			if time.Since(started) > time.Second {
				t.Fatal("request blocked behind abandoned reader")
			}
		})
	}
}

func TestConnectCancellation(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	closed := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			closed <- err
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		b := make([]byte, 4096)
		for {
			if _, err := conn.Read(b); err != nil {
				closed <- err
				return
			}
		}
	}()
	client := New(listener.Addr().String(), time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := client.Connect(ctx); err == nil {
		client.Close()
		t.Fatal("connected to stalled TLS server")
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("dial connection leaked")
	}
}
