// Package cloudflare manages DNS records idempotently (FR-21).
package cloudflare

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
	ErrUnavailable = errors.New("cloudflare unavailable")
	// ErrConflict means a different record already exists; we never overwrite it.
	ErrConflict = errors.New("cloudflare: conflicting record exists")
	ErrNoZone   = errors.New("cloudflare: zone not found for domain")
)

type Record struct {
	Type     string
	Name     string
	Content  string
	Priority int // MX only
	// MatchPrefix identifies "our" TXT kind (e.g. "v=spf1") so an existing
	// record of the same kind with different content is reported as conflict.
	MatchPrefix string
}

type API interface {
	// Upsert creates the record, or updates it when knownID is set. If an
	// identical record already exists it is adopted. Returns the record id.
	Upsert(ctx context.Context, domain string, r Record, knownID string) (string, error)
	Delete(ctx context.Context, domain, id string) error
}

type Client struct {
	base   string
	token  string
	zoneID string
	hc     *http.Client
}

func New(token, zoneID string) *Client {
	return &Client{base: "https://api.cloudflare.com/client/v4", token: token, zoneID: zoneID, hc: &http.Client{Timeout: 15 * time.Second}}
}

// WithBase is used by tests.
func (c *Client) WithBase(b string) *Client { c.base = b; return c }

type cfResp struct {
	Success bool `json:"success"`
	Errors  []struct {
		Message string `json:"message"`
	} `json:"errors"`
	Result json.RawMessage `json:"result"`
}

func (c *Client) call(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 500 || resp.StatusCode == 429 {
		return fmt.Errorf("%w: status %d", ErrUnavailable, resp.StatusCode)
	}
	var r cfResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("cloudflare: bad response (status %d)", resp.StatusCode)
	}
	if !r.Success {
		msg := "unknown error"
		if len(r.Errors) > 0 {
			msg = r.Errors[0].Message
		}
		return fmt.Errorf("cloudflare: %s (status %d)", msg, resp.StatusCode)
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

func (c *Client) zone(ctx context.Context, domain string) (string, error) {
	if c.zoneID != "" {
		return c.zoneID, nil
	}
	// Walk up the labels: sub.example.com -> example.com
	parts := strings.Split(domain, ".")
	for i := 0; i < len(parts)-1; i++ {
		name := strings.Join(parts[i:], ".")
		var zs []struct {
			ID string `json:"id"`
		}
		if err := c.call(ctx, http.MethodGet, "/zones?name="+url.QueryEscape(name), nil, &zs); err != nil {
			return "", err
		}
		if len(zs) > 0 {
			return zs[0].ID, nil
		}
	}
	return "", ErrNoZone
}

type cfRecord struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Name     string `json:"name"`
	Content  string `json:"content"`
	Priority int    `json:"priority,omitempty"`
	TTL      int    `json:"ttl"`
}

func norm(s string) string { return strings.Trim(strings.ToLower(strings.TrimSpace(s)), `".`) }

func (c *Client) Upsert(ctx context.Context, domain string, r Record, knownID string) (string, error) {
	zid, err := c.zone(ctx, domain)
	if err != nil {
		return "", err
	}
	payload := cfRecord{Type: r.Type, Name: r.Name, Content: r.Content, TTL: 1}
	if r.Type == "MX" {
		payload.Priority = r.Priority
	}
	if r.Type == "TXT" {
		payload.Content = `"` + strings.ReplaceAll(r.Content, `"`, `\"`) + `"`
	}
	if knownID != "" {
		var out cfRecord
		err := c.call(ctx, http.MethodPut, "/zones/"+zid+"/dns_records/"+knownID, payload, &out)
		if err == nil {
			return out.ID, nil
		}
		// record vanished upstream -> fall through to search/create
		if !strings.Contains(err.Error(), "status 404") {
			return "", err
		}
	}
	var existing []cfRecord
	q := "/zones/" + zid + "/dns_records?type=" + r.Type + "&name=" + url.QueryEscape(r.Name) + "&per_page=100"
	if err := c.call(ctx, http.MethodGet, q, nil, &existing); err != nil {
		return "", err
	}
	for _, e := range existing {
		if norm(e.Content) == norm(r.Content) {
			return e.ID, nil // idempotent adoption
		}
	}
	for _, e := range existing {
		// Different record of the same kind: report, never overwrite (FR-21).
		if r.Type == "MX" || (r.MatchPrefix != "" && strings.HasPrefix(norm(e.Content), strings.ToLower(r.MatchPrefix))) {
			return "", fmt.Errorf("%w: %s %s currently %q", ErrConflict, e.Type, e.Name, e.Content)
		}
	}
	var out cfRecord
	if err := c.call(ctx, http.MethodPost, "/zones/"+zid+"/dns_records", payload, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

func (c *Client) Delete(ctx context.Context, domain, id string) error {
	zid, err := c.zone(ctx, domain)
	if err != nil {
		return err
	}
	err = c.call(ctx, http.MethodDelete, "/zones/"+zid+"/dns_records/"+id, nil, nil)
	if err != nil && strings.Contains(err.Error(), "status 404") {
		return nil
	}
	return err
}
