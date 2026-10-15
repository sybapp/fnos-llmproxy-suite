package fnos

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
}

func dialWebSocket(server string, timeout time.Duration) (*wsConn, error) {
	server = strings.TrimSpace(server)
	server = strings.TrimPrefix(server, "https://")
	server = strings.TrimPrefix(server, "http://")
	server = strings.TrimPrefix(server, "wss://")
	server = strings.TrimPrefix(server, "ws://")
	server = strings.TrimSuffix(server, "/")
	if server == "" {
		return nil, errors.New("empty fnOS server")
	}

	host := server
	if i := strings.IndexRune(host, '/'); i >= 0 {
		host = host[:i]
	}
	hostName := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		hostName = h
	} else {
		host += ":443"
	}

	d := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
	raw, err := d.Dial("tcp", host)
	if err != nil {
		return nil, err
	}
	tlsConn := tls.Client(raw, &tls.Config{ServerName: strings.Trim(hostName, "[]"), MinVersion: tls.VersionTLS12})
	if err := tlsConn.SetDeadline(time.Now().Add(timeout)); err != nil {
		_ = raw.Close()
		return nil, err
	}
	if err := tlsConn.Handshake(); err != nil {
		_ = raw.Close()
		return nil, err
	}

	keyRaw := make([]byte, 16)
	if _, err := rand.Read(keyRaw); err != nil {
		_ = tlsConn.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyRaw)
	req := fmt.Sprintf("GET /websocket?type=main HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\nCookie: mode=relay; language=zh\r\nUser-Agent: fnconnect-proxy/1.0\r\n\r\n", strings.TrimSuffix(host, ":443"), key)
	if _, err := io.WriteString(tlsConn, req); err != nil {
		_ = tlsConn.Close()
		return nil, err
	}

	br := bufio.NewReader(tlsConn)
	resp, err := http.ReadResponse(br, &http.Request{Method: "GET"})
	if err != nil {
		_ = tlsConn.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		_ = tlsConn.Close()
		return nil, fmt.Errorf("websocket upgrade failed: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	wantRaw := sha1.Sum([]byte(key + wsGUID))
	want := base64.StdEncoding.EncodeToString(wantRaw[:])
	if got := resp.Header.Get("Sec-WebSocket-Accept"); got != want {
		_ = tlsConn.Close()
		return nil, errors.New("invalid websocket accept header")
	}
	_ = tlsConn.SetDeadline(time.Time{})
	return &wsConn{conn: tlsConn, br: br}, nil
}

func (w *wsConn) Close() error { return w.conn.Close() }

func (w *wsConn) writeFrame(opcode byte, payload []byte) error {
	if len(payload) > 64<<20 {
		return errors.New("websocket frame too large")
	}
	var h bytes.Buffer
	h.WriteByte(0x80 | (opcode & 0x0f))
	n := len(payload)
	switch {
	case n < 126:
		h.WriteByte(byte(0x80 | n))
	case n <= 65535:
		h.WriteByte(0x80 | 126)
		_ = binary.Write(&h, binary.BigEndian, uint16(n))
	default:
		h.WriteByte(0x80 | 127)
		_ = binary.Write(&h, binary.BigEndian, uint64(n))
	}
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	h.Write(mask)
	masked := make([]byte, n)
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	if _, err := w.conn.Write(h.Bytes()); err != nil {
		return err
	}
	_, err := w.conn.Write(masked)
	return err
}

func (w *wsConn) WriteText(s string) error { return w.writeFrame(0x1, []byte(s)) }

func (w *wsConn) readFrame() (fin bool, opcode byte, payload []byte, err error) {
	b1, err := w.br.ReadByte()
	if err != nil {
		return false, 0, nil, err
	}
	b2, err := w.br.ReadByte()
	if err != nil {
		return false, 0, nil, err
	}
	fin = b1&0x80 != 0
	opcode = b1 & 0x0f
	masked := b2&0x80 != 0
	n := uint64(b2 & 0x7f)
	if n == 126 {
		var x uint16
		if err := binary.Read(w.br, binary.BigEndian, &x); err != nil {
			return false, 0, nil, err
		}
		n = uint64(x)
	} else if n == 127 {
		if err := binary.Read(w.br, binary.BigEndian, &n); err != nil {
			return false, 0, nil, err
		}
	}
	if n > 64<<20 {
		return false, 0, nil, errors.New("websocket message too large")
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(w.br, mask[:]); err != nil {
			return false, 0, nil, err
		}
	}
	payload = make([]byte, int(n))
	if _, err := io.ReadFull(w.br, payload); err != nil {
		return false, 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return fin, opcode, payload, nil
}

func (w *wsConn) ReadMessage() ([]byte, error) {
	var out bytes.Buffer
	var started bool
	for {
		fin, op, p, err := w.readFrame()
		if err != nil {
			return nil, err
		}
		switch op {
		case 0x8:
			_ = w.writeFrame(0x8, nil)
			return nil, io.EOF
		case 0x9:
			_ = w.writeFrame(0xA, p)
			continue
		case 0xA:
			continue
		case 0x1, 0x2:
			if started {
				return nil, errors.New("unexpected new websocket data frame")
			}
			started = true
			out.Write(p)
		case 0x0:
			if !started {
				return nil, errors.New("unexpected websocket continuation")
			}
			out.Write(p)
		default:
			continue
		}
		if started && fin {
			return out.Bytes(), nil
		}
	}
}
