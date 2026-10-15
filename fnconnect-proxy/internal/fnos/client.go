package fnos

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Session struct {
	FNOSToken  string
	LongToken  string
	EntryToken string
	SignKey    string
	BackID     string
	UID        int64
	Admin      bool
}

type Client struct {
	server  string
	timeout time.Duration
	ws      *wsConn
	aesKey  string
	iv      []byte
	pub     string
	si      string
	signKey string
	backID  string
	index   uint32
	reqMu   sync.Mutex
}

func New(server string, timeout time.Duration) *Client {
	return &Client{server: server, timeout: timeout, backID: "0000000000000000"}
}

func (c *Client) Connect(ctx context.Context) error {
	key, err := randomString(32)
	if err != nil {
		return err
	}
	iv, err := randomBytes(16)
	if err != nil {
		return err
	}
	type result struct {
		w   *wsConn
		err error
	}
	ch := make(chan result, 1)
	go func() { w, e := dialWebSocket(c.server, c.timeout); ch <- result{w, e} }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case r := <-ch:
		if r.err != nil {
			return r.err
		}
		c.ws, c.aesKey, c.iv = r.w, key, iv
		return nil
	}
}

func (c *Client) Close() error {
	if c.ws != nil {
		return c.ws.Close()
	}
	return nil
}

func (c *Client) reqID() string {
	c.index++
	ts := strconv.FormatInt(time.Now().Unix(), 16)
	if len(ts) < 8 {
		ts = strings.Repeat("0", 8-len(ts)) + ts
	}
	idx := fmt.Sprintf("%04x", c.index)
	back := c.backID
	if back == "" {
		back = "0000000000000000"
	}
	return ts + back + idx
}

func fieldString(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}
func fieldBool(m map[string]any, k string) bool {
	if v, ok := m[k].(bool); ok {
		return v
	}
	return false
}
func fieldInt64(m map[string]any, k string) int64 {
	switch v := m[k].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case json.Number:
		x, _ := v.Int64()
		return x
	}
	return 0
}

func hasErrno(m map[string]any) bool { _, ok := m["errno"]; return ok }
func responseError(prefix string, m map[string]any) error {
	if !hasErrno(m) {
		return nil
	}
	msg := fieldString(m, "msg")
	if msg == "" {
		msg = fieldString(m, "error")
	}
	if msg == "" {
		b, _ := json.Marshal(m)
		msg = string(b)
	}
	return fmt.Errorf("%s: %s", prefix, msg)
}

func (c *Client) request(ctx context.Context, req string, params map[string]any, encrypt bool) (map[string]any, error) {
	c.reqMu.Lock()
	defer c.reqMu.Unlock()
	if c.ws == nil {
		return nil, errors.New("not connected")
	}
	reqID := c.reqID()
	data := map[string]any{"req": req, "reqid": reqID}
	for k, v := range params {
		data[k] = v
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	var message string
	if encrypt {
		if c.pub == "" {
			return nil, errors.New("RSA public key not loaded")
		}
		rsaCT, err := rsaEncryptPKCS1v15(c.pub, c.aesKey)
		if err != nil {
			return nil, err
		}
		aesCT, err := aesCBCEncrypt(raw, c.aesKey, c.iv)
		if err != nil {
			return nil, err
		}
		outer := map[string]any{"req": "encrypted", "iv": base64.StdEncoding.EncodeToString(c.iv), "rsa": rsaCT, "aes": aesCT}
		b, _ := json.Marshal(outer)
		message = string(b)
	} else {
		if c.signKey != "" && req != "util.getSI" && req != "util.crypto.getRSAPub" && req != "ping" {
			sig, err := hmacSignature(raw, c.signKey)
			if err != nil {
				return nil, err
			}
			message = sig + string(raw)
		} else {
			message = string(raw)
		}
	}
	if err := c.ws.WriteText(message); err != nil {
		return nil, err
	}

	deadline := time.NewTimer(c.timeout)
	defer deadline.Stop()
	for {
		type rr struct {
			b   []byte
			err error
		}
		ch := make(chan rr, 1)
		go func() { b, e := c.ws.ReadMessage(); ch <- rr{b, e} }()
		var b []byte
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("request timeout: %s", req)
		case r := <-ch:
			if r.err != nil {
				return nil, r.err
			}
			b = r.b
		}
		var m map[string]any
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		if fieldString(m, "reqid") == reqID {
			return m, nil
		}
	}
}

func (c *Client) GetRSAPub(ctx context.Context) error {
	m, err := c.request(ctx, "util.crypto.getRSAPub", nil, false)
	if err != nil {
		return err
	}
	if err := responseError("get RSA pub failed", m); err != nil {
		return err
	}
	c.pub, c.si = fieldString(m, "pub"), fieldString(m, "si")
	if c.pub == "" {
		return errors.New("fnOS did not return RSA public key")
	}
	return nil
}

func (c *Client) Login(ctx context.Context, username, password, deviceType, deviceName string) (Session, error) {
	if c.pub == "" {
		if err := c.GetRSAPub(ctx); err != nil {
			return Session{}, err
		}
	}
	m, err := c.request(ctx, "user.login", map[string]any{"user": username, "password": password, "stay": true, "deviceType": deviceType, "deviceName": deviceName, "si": c.si}, true)
	if err != nil {
		return Session{}, err
	}
	if err := responseError("login failed", m); err != nil {
		return Session{}, err
	}
	c.backID = fieldString(m, "backId")
	secret := fieldString(m, "secret")
	if secret == "" {
		return Session{}, errors.New("login response missing secret")
	}
	sign, err := decryptSecret(secret, c.aesKey, c.iv)
	if err != nil {
		return Session{}, fmt.Errorf("decrypt login sign key: %w", err)
	}
	c.signKey = sign
	sess := Session{FNOSToken: fieldString(m, "token"), LongToken: fieldString(m, "longToken"), SignKey: sign, BackID: c.backID, UID: fieldInt64(m, "uid"), Admin: fieldBool(m, "admin")}
	if sess.FNOSToken == "" {
		return Session{}, errors.New("login response missing fnos token")
	}
	em, err := c.request(ctx, "appcgi.sac.entry.v1.exchangeEntryToken", nil, false)
	if err != nil {
		return Session{}, err
	}
	if err := responseError("exchange entry-token failed", em); err != nil {
		return Session{}, err
	}
	if d, ok := em["data"].(map[string]any); ok {
		sess.EntryToken = fieldString(d, "token")
	}
	if sess.EntryToken == "" {
		sess.EntryToken = fieldString(em, "token")
	}
	if sess.EntryToken == "" {
		return Session{}, errors.New("entry-token missing from response")
	}
	return sess, nil
}

func (c *Client) RefreshWithFNOSToken(ctx context.Context, fnosToken, signKey, backID string) (Session, error) {
	c.signKey, c.backID = signKey, backID
	if err := c.GetRSAPub(ctx); err != nil {
		return Session{}, err
	}
	m, err := c.request(ctx, "user.authToken", map[string]any{"token": fnosToken, "main": true, "si": c.si}, false)
	if err != nil {
		return Session{}, err
	}
	if err := responseError("fnos token auth failed", m); err != nil {
		return Session{}, err
	}
	if x := fieldString(m, "backId"); x != "" {
		c.backID = x
	}
	em, err := c.request(ctx, "appcgi.sac.entry.v1.exchangeEntryToken", nil, false)
	if err != nil {
		return Session{}, err
	}
	if err := responseError("exchange entry-token failed", em); err != nil {
		return Session{}, err
	}
	tok := ""
	if d, ok := em["data"].(map[string]any); ok {
		tok = fieldString(d, "token")
	}
	if tok == "" {
		tok = fieldString(em, "token")
	}
	if tok == "" {
		return Session{}, errors.New("entry-token missing from response")
	}
	return Session{FNOSToken: fnosToken, EntryToken: tok, SignKey: signKey, BackID: c.backID, UID: fieldInt64(m, "uid"), Admin: fieldBool(m, "admin")}, nil
}

func makeDID() (string, error) {
	rnd, err := randomBytes(24)
	if err != nil {
		return "", err
	}
	return strings.ToLower(base64.RawURLEncoding.EncodeToString(rnd)), nil
}

func (c *Client) RefreshWithLongToken(ctx context.Context, longToken, signKey, backID, deviceType, deviceName string) (Session, error) {
	c.signKey = signKey
	c.backID = backID
	if err := c.GetRSAPub(ctx); err != nil {
		return Session{}, err
	}
	did, err := makeDID()
	if err != nil {
		return Session{}, err
	}
	m, err := c.request(ctx, "user.tokenLogin", map[string]any{
		"token": longToken, "deviceType": deviceType, "deviceName": deviceName,
		"did": did, "si": c.si,
	}, false)
	if err != nil {
		return Session{}, err
	}
	if err := responseError("long-token login failed", m); err != nil {
		return Session{}, err
	}
	if x := fieldString(m, "backId"); x != "" {
		c.backID = x
	}
	sess := Session{FNOSToken: fieldString(m, "token"), LongToken: longToken, SignKey: signKey, BackID: c.backID, UID: fieldInt64(m, "uid"), Admin: fieldBool(m, "admin")}
	if x := fieldString(m, "longToken"); x != "" {
		sess.LongToken = x
	}
	if sess.FNOSToken == "" {
		return Session{}, errors.New("tokenLogin response missing fnos token")
	}
	em, err := c.request(ctx, "appcgi.sac.entry.v1.exchangeEntryToken", nil, false)
	if err != nil {
		return Session{}, err
	}
	if err := responseError("exchange entry-token failed", em); err != nil {
		return Session{}, err
	}
	if d, ok := em["data"].(map[string]any); ok {
		sess.EntryToken = fieldString(d, "token")
	}
	if sess.EntryToken == "" {
		sess.EntryToken = fieldString(em, "token")
	}
	if sess.EntryToken == "" {
		return Session{}, errors.New("entry-token missing from response")
	}
	return sess, nil
}
