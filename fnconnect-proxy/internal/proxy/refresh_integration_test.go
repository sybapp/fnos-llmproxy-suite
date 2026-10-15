package proxy

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	appcfg "fnconnect-proxy/internal/config"
)

// The child process uses a fresh system certificate pool trusting only this
// test's TLS fixture. Production certificate verification stays enabled.
func TestLocalTLSRefreshWorkflow(t *testing.T) {
	if path := os.Getenv("FNCONNECT_REFRESH_TEST_CONFIG"); path != "" {
		cfg, err := appcfg.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		originalExpiry := cfg.LongTokenExpires
		mgr := NewTokenManager(path, cfg)
		if os.Getenv("FNCONNECT_REFRESH_TEST_MODE") == "manual-refresh" {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := mgr.RenewSession(ctx); err != nil {
				t.Fatal(err)
			}
		}
		handler, err := NewServer(mgr, cfg.TargetURL)
		if err != nil {
			t.Fatal(err)
		}
		requests := 2
		if os.Getenv("FNCONNECT_REFRESH_TEST_MODE") == "always-rejected" {
			requests = 1
		}
		for i := 0; i < requests; i++ {
			response := httptest.NewRecorder()
			req := httptest.NewRequest("POST", "http://local/v1/models", strings.NewReader(`{"probe":true}`))
			req.Header.Set("Authorization", "Bearer llm-test-key")
			handler.ServeHTTP(response, req)
			if os.Getenv("FNCONNECT_REFRESH_TEST_MODE") == "always-rejected" {
				if response.Code != 502 || !strings.Contains(response.Body.String(), "rejected the renewed session") {
					t.Fatalf("unexpected recovery failure: %d %s", response.Code, response.Body.String())
				}
				continue
			}
			if response.Code != 200 || strings.TrimSpace(response.Body.String()) != `{"models":["test-model"]}` {
				t.Fatalf("refreshed session ineffective: %d %s", response.Code, response.Body.String())
			}
			if i == 0 {
				persisted, err := appcfg.Load(path)
				if err != nil {
					t.Fatal(err)
				}
				handler, err = NewServer(NewTokenManager(path, persisted), persisted.TargetURL)
				if err != nil {
					t.Fatal(err)
				}
			}
		}
		saved, err := appcfg.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if saved.FNOSToken != "new-fnos" || saved.EntryToken != "new-entry" {
			t.Fatal("refreshed tokens were not persisted")
		}
		if !saved.LongTokenExpires.Equal(originalExpiry) {
			t.Fatal("unchanged longToken expiry extended")
		}
		return
	}
	for _, mode := range []string{"auth-rejected", "gateway-rejected", "transport-failure", "manual-refresh", "near-expiry", "always-rejected"} {
		t.Run(mode, func(t *testing.T) {
			key := base64.StdEncoding.EncodeToString([]byte("fixture-established-signing-key"))
			var renewals, authAttempts, gatewayRejections atomic.Int32
			fixture := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/websocket" {
					if r.URL.Path != "/app/llmproxy/v1/models" {
						t.Errorf("wrong target path: %s", r.URL.Path)
					}
					if r.Header.Get("Authorization") != "Bearer llm-test-key" {
						t.Error("LLM authorization changed")
					}
					payload, _ := io.ReadAll(r.Body)
					if string(payload) != `{"probe":true}` {
						t.Errorf("request body lost on retry: %s", payload)
					}
					fnosToken, _ := r.Cookie("fnos-token")
					entryToken, _ := r.Cookie("entry-token")
					if _, err := r.Cookie("fnos-long-token"); err == nil {
						t.Error("longToken forwarded")
					}
					if mode == "always-rejected" || fnosToken == nil || entryToken == nil || fnosToken.Value != "new-fnos" || entryToken.Value != "new-entry" {
						gatewayRejections.Add(1)
						w.Header().Set("Content-Type", "text/plain")
						fmt.Fprint(w, "invalid token")
						return
					}
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprint(w, `{"models":["test-model"]}`)
					return
				}
				conn, rw, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				accept := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
				fmt.Fprintf(rw, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n", base64.StdEncoding.EncodeToString(accept[:]))
				if err := rw.Flush(); err != nil {
					t.Error(err)
					return
				}
				renewed, rejected := false, false
				for {
					b, err := readFixtureFrame(rw.Reader)
					if err != nil {
						return
					}
					raw := b
					signed := len(b) > 0 && b[0] != '{'
					if signed {
						if len(b) < 44 {
							t.Error("short signature")
							return
						}
						raw = b[44:]
						mac := hmac.New(sha256.New, []byte("fixture-established-signing-key"))
						mac.Write(raw)
						if string(b[:44]) != base64.StdEncoding.EncodeToString(mac.Sum(nil)) {
							t.Error("invalid signing key")
							return
						}
					}
					var req map[string]any
					if err := json.Unmarshal(raw, &req); err != nil {
						t.Error(err)
						return
					}
					name, _ := req["req"].(string)
					response := map[string]any{"reqid": req["reqid"]}
					switch name {
					case "util.crypto.getRSAPub":
						response["pub"] = "unused-for-signed-auth"
						response["si"] = "fixture-si"
					case "user.authToken":
						if !signed {
							t.Error("unsigned authToken")
							return
						}
						authAttempts.Add(1)
						if mode == "transport-failure" {
							return
						}
						if mode == "auth-rejected" {
							response["errno"] = 135168
							response["msg"] = "expired"
							rejected = true
						} else {
							response["backId"] = "old-back"
						}
					case "user.tokenLogin":
						if !signed || req["token"] != "fixture-long" {
							t.Error("invalid longToken login")
							return
						}
						if rejected {
							t.Error("fallback reused rejected socket")
							return
						}
						renewals.Add(1)
						renewed = true
						response["token"] = "new-fnos"
						response["backId"] = "new-back"
						response["secret"] = "opaque-secret-without-an-AES-handshake"
					case "appcgi.sac.entry.v1.exchangeEntryToken":
						if !signed {
							t.Error("unsigned entry exchange")
							return
						}
						token := "old-refreshed-entry"
						if renewed {
							token = "new-entry"
						}
						response["data"] = map[string]any{"token": token}
					default:
						t.Errorf("unexpected request: %s", name)
						return
					}
					b, _ = json.Marshal(response)
					if err := writeFixtureFrame(conn, b); err != nil {
						return
					}
				}
			}))
			defer fixture.Close()
			dir := t.TempDir()
			certPath := filepath.Join(dir, "fixture.pem")
			if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: fixture.Certificate().Raw}), 0600); err != nil {
				t.Fatal(err)
			}
			cfg := appcfg.Default()
			cfg.Server = strings.TrimPrefix(fixture.URL, "https://")
			cfg.TargetURL = fixture.URL + "/app/llmproxy"
			cfg.FNOSToken = "old-fnos"
			cfg.EntryToken = "old-entry"
			cfg.LongToken = "fixture-long"
			cfg.SignKey = key
			cfg.BackID = "old-back"
			cfg.FNOSTokenExpires = time.Now().Add(6 * time.Hour)
			cfg.EntryTokenExpires = time.Now().Add(6 * time.Hour)
			cfg.LongTokenExpires = time.Now().Add(20 * 24 * time.Hour)
			if mode == "auth-rejected" || mode == "transport-failure" {
				cfg.EntryTokenExpires = time.Now().Add(-time.Hour)
			}
			if mode == "near-expiry" {
				cfg.FNOSTokenExpires = time.Now().Add(10 * time.Minute)
			}
			path := filepath.Join(dir, "config.json")
			if err := appcfg.Save(path, cfg); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestLocalTLSRefreshWorkflow$", "-test.v")
			cmd.Env = append(os.Environ(), "SSL_CERT_FILE="+certPath, "SSL_CERT_DIR="+dir, "FNCONNECT_REFRESH_TEST_CONFIG="+path, "FNCONNECT_REFRESH_TEST_MODE="+mode)
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("local TLS workflow: %v\n%s", err, output)
			}
			if renewals.Load() != 1 {
				t.Fatalf("longToken renewals=%d", renewals.Load())
			}
			if mode == "gateway-rejected" && (authAttempts.Load() != 1 || gatewayRejections.Load() != 2) {
				t.Fatalf("missing bounded gateway recovery: auth=%d rejections=%d", authAttempts.Load(), gatewayRejections.Load())
			}
			if mode == "always-rejected" {
				if gatewayRejections.Load() != 3 {
					t.Fatalf("unbounded recovery: %d gateway attempts", gatewayRejections.Load())
				}
				t.Log("persistent gateway rejection stops after two retries with a clear error")
			} else {
				t.Log("signed tokenLogin and entry exchange succeeded; renewed cookies passed TLS gateway and persisted session passed again")
			}
		})
	}
}

func readFixtureFrame(r *bufio.Reader) ([]byte, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	n := uint64(header[1] & 127)
	switch n {
	case 126:
		var size uint16
		if err := binary.Read(r, binary.BigEndian, &size); err != nil {
			return nil, err
		}
		n = uint64(size)
	case 127:
		if err := binary.Read(r, binary.BigEndian, &n); err != nil {
			return nil, err
		}
	}
	if n > 1<<20 {
		return nil, fmt.Errorf("oversized fixture frame")
	}
	mask := make([]byte, 4)
	if header[1]&128 != 0 {
		if _, err := io.ReadFull(r, mask); err != nil {
			return nil, err
		}
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	if header[1]&128 != 0 {
		for i := range b {
			b[i] ^= mask[i%4]
		}
	}
	return b, nil
}
func writeFixtureFrame(w io.Writer, b []byte) error {
	header := []byte{0x81}
	if len(b) < 126 {
		header = append(header, byte(len(b)))
	} else {
		header = append(header, 126, byte(len(b)>>8), byte(len(b)))
	}
	_, err := w.Write(append(header, b...))
	return err
}
