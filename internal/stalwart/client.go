// Package stalwart is the HTTP client for Stalwart's management REST API (FR-20).
// SelfMail never implements SMTP/IMAP itself; everything is delegated here.
package stalwart

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	ErrUnavailable = errors.New("stalwart unavailable")
	ErrNotFound    = errors.New("stalwart: not found")
	ErrConflict    = errors.New("stalwart: already exists")
)

// API is what the rest of the app depends on, so it can be faked in tests.
type API interface {
	Ping(ctx context.Context) error
	EnsureDomain(ctx context.Context, name string) (id string, err error)
	DeleteDomain(ctx context.Context, name string) error
	// EnsureDKIM creates (or reuses) the signing key and returns the public
	// key (base64, as used in the p= tag) for the selector.
	EnsureDKIM(ctx context.Context, domain, selector string) (publicKey string, err error)
	CreateMailbox(ctx context.Context, address, password string, quotaBytes int64) (id string, err error)
	MailboxExists(ctx context.Context, address string) (bool, error)
	SetQuota(ctx context.Context, address string, quotaBytes int64) error
	SetPassword(ctx context.Context, address, password string) error
	// DeleteMailbox returns the number of messages that were removed (best effort).
	DeleteMailbox(ctx context.Context, address string) (deletedMessages int64, err error)
	Usage(ctx context.Context, address string) (usedBytes int64, err error)
	// SetForwarder registers (or replaces) a multi-target alias. Pass the alias
	// local address ("info@x.com") or catch-all ("@x.com").
	SetForwarder(ctx context.Context, address string, targets []string) error
	DeleteForwarder(ctx context.Context, address string) error
	Learn(ctx context.Context, kind string, rawMessage []byte) error
	ApplySettings(ctx context.Context, kv map[string]string) error
}

type Client struct {
	base    string
	user    string
	pass    string
	hc      *http.Client
	retries int
}

func New(baseURL, user, pass string, timeout time.Duration) *Client {
	return &Client{
		base: strings.TrimRight(baseURL, "/"), user: user, pass: pass,
		hc: &http.Client{Timeout: timeout}, retries: 2,
	}
}

type envelope struct {
	Data  json.RawMessage `json:"data"`
	Error string          `json:"error"`
	Item  string          `json:"item"`
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if b, ok := body.([]byte); ok {
			payload = b
		} else if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	attempts := 1
	if method == http.MethodGet {
		attempts += c.retries // only idempotent reads are retried automatically
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("%w: %v", ErrUnavailable, ctx.Err())
			case <-time.After(time.Duration(i) * 200 * time.Millisecond):
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		if _, raw := body.([]byte); !raw {
			req.Header.Set("Content-Type", "application/json")
		}
		if c.user != "" {
			req.SetBasicAuth(c.user, c.pass)
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("%w: %v", ErrUnavailable, err)
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		switch {
		case resp.StatusCode >= 500:
			lastErr = fmt.Errorf("%w: status %d", ErrUnavailable, resp.StatusCode)
			continue
		case resp.StatusCode == http.StatusNotFound:
			return ErrNotFound
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			return fmt.Errorf("stalwart rejected credentials (status %d)", resp.StatusCode)
		case resp.StatusCode >= 400:
			return fmt.Errorf("stalwart: status %d: %s", resp.StatusCode, truncate(string(raw)))
		}
		var env envelope
		if len(raw) > 0 && json.Unmarshal(raw, &env) == nil {
			switch env.Error {
			case "":
			case "fieldAlreadyExists":
				return ErrConflict
			case "notFound":
				return ErrNotFound
			default:
				return fmt.Errorf("stalwart error %q (%s)", env.Error, env.Item)
			}
			if out != nil && len(env.Data) > 0 {
				return json.Unmarshal(env.Data, out)
			}
		}
		return nil
	}
	return lastErr
}

func truncate(s string) string {
	if len(s) > 300 {
		return s[:300]
	}
	return s
}

func (c *Client) Ping(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/api/principal?limit=1", nil, nil)
}

type principal struct {
	ID        json.Number `json:"id"`
	Type      string      `json:"type"`
	Name      string      `json:"name"`
	UsedQuota int64       `json:"usedQuota"`
	Quota     int64       `json:"quota"`
	Emails    []string    `json:"emails"`
	Members   []string    `json:"members"`
}

func (c *Client) getPrincipal(ctx context.Context, name string) (*principal, error) {
	var p principal
	if err := c.do(ctx, http.MethodGet, "/api/principal/"+url.PathEscape(name), nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (c *Client) EnsureDomain(ctx context.Context, name string) (string, error) {
	err := c.do(ctx, http.MethodPost, "/api/principal", map[string]any{"type": "domain", "name": name}, nil)
	if err != nil && !errors.Is(err, ErrConflict) { // idempotent (FR-14)
		return "", err
	}
	return name, nil
}

func (c *Client) DeleteDomain(ctx context.Context, name string) error {
	err := c.do(ctx, http.MethodDelete, "/api/principal/"+url.PathEscape(name), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (c *Client) EnsureDKIM(ctx context.Context, domain, selector string) (string, error) {
	err := c.do(ctx, http.MethodPost, "/api/dkim", map[string]any{
		"id": nil, "algorithm": "RSA", "domain": domain, "selector": selector,
	}, nil)
	if err != nil && !errors.Is(err, ErrConflict) {
		return "", err
	}
	return c.dkimPublicKey(ctx, domain, selector)
}

func (c *Client) dkimPublicKey(ctx context.Context, domain, selector string) (string, error) {
	var recs []struct {
		Type    string `json:"type"`
		Name    string `json:"name"`
		Content string `json:"content"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/dns/records/"+url.PathEscape(domain), nil, &recs); err != nil {
		return "", err
	}
	want := strings.ToLower(selector + "._domainkey")
	for _, r := range recs {
		if r.Type == "TXT" && strings.HasPrefix(strings.ToLower(r.Name), want) {
			for _, part := range strings.Split(r.Content, ";") {
				kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
				if len(kv) == 2 && kv[0] == "p" {
					return strings.Join(strings.Fields(kv[1]), ""), nil
				}
			}
		}
	}
	return "", fmt.Errorf("stalwart returned no DKIM public key for %s", selector)
}

func (c *Client) CreateMailbox(ctx context.Context, address, password string, quota int64) (string, error) {
	err := c.do(ctx, http.MethodPost, "/api/principal", map[string]any{
		"type": "individual", "name": address, "secrets": []string{password},
		"emails": []string{address}, "quota": quota, "description": address,
	}, nil)
	if err != nil {
		return "", err
	}
	return address, nil
}

func (c *Client) MailboxExists(ctx context.Context, address string) (bool, error) {
	_, err := c.getPrincipal(ctx, address)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

func patch(field string, value any) []map[string]any {
	return []map[string]any{{"action": "set", "field": field, "value": value}}
}

func (c *Client) SetQuota(ctx context.Context, address string, quota int64) error {
	return c.do(ctx, http.MethodPatch, "/api/principal/"+url.PathEscape(address), patch("quota", quota), nil)
}

func (c *Client) SetPassword(ctx context.Context, address, password string) error {
	// "secrets" replaces all secrets, which also invalidates old sessions.
	return c.do(ctx, http.MethodPatch, "/api/principal/"+url.PathEscape(address), patch("secrets", []string{password}), nil)
}

func (c *Client) DeleteMailbox(ctx context.Context, address string) (int64, error) {
	// Stalwart does not report a message count on deletion; usage in bytes is
	// the best signal we have, so the count is derived by the caller from the
	// message count endpoint when available. We return 0 when unknown.
	var count int64
	var st struct {
		Messages int64 `json:"messages"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/principal/"+url.PathEscape(address)+"/stats", nil, &st); err == nil {
		count = st.Messages
	}
	err := c.do(ctx, http.MethodDelete, "/api/principal/"+url.PathEscape(address), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return count, nil
	}
	return count, err
}

func (c *Client) Usage(ctx context.Context, address string) (int64, error) {
	p, err := c.getPrincipal(ctx, address)
	if err != nil {
		return 0, err
	}
	return p.UsedQuota, nil
}

func (c *Client) SetForwarder(ctx context.Context, address string, targets []string) error {
	name := "fwd:" + address
	_ = c.do(ctx, http.MethodDelete, "/api/principal/"+url.PathEscape(name), nil, nil) // replace semantics
	return c.do(ctx, http.MethodPost, "/api/principal", map[string]any{
		"type": "list", "name": name, "emails": []string{address}, "members": targets,
	}, nil)
}

func (c *Client) DeleteForwarder(ctx context.Context, address string) error {
	err := c.do(ctx, http.MethodDelete, "/api/principal/"+url.PathEscape("fwd:"+address), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

func (c *Client) Learn(ctx context.Context, kind string, raw []byte) error {
	if kind != "ham" && kind != "spam" {
		return fmt.Errorf("invalid learn kind %q", kind)
	}
	return c.do(ctx, http.MethodPost, "/api/spam-filter/train/"+kind, raw, nil)
}

func (c *Client) ApplySettings(ctx context.Context, kv map[string]string) error {
	values := make([][2]string, 0, len(kv))
	for k, v := range kv {
		values = append(values, [2]string{k, v})
	}
	return c.do(ctx, http.MethodPost, "/api/settings",
		[]map[string]any{{"type": "insert", "prefix": nil, "values": values, "assert_empty": false}}, nil)
}
