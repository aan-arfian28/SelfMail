// Package stalwart is the client for Stalwart's JMAP management API
// (Stalwart >= 1.x: x:Domain, x:Account, x:MailingList objects) (FR-20).
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
	"regexp"
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
	// DomainDKIM returns the active RSA DKIM selector and public key (p= value)
	// that Stalwart generated for the domain.
	DomainDKIM(ctx context.Context, domain string) (selector, publicKey string, err error)
	CreateMailbox(ctx context.Context, address, password string, quotaBytes int64) (id string, err error)
	SetQuota(ctx context.Context, address string, quotaBytes int64) error
	SetPassword(ctx context.Context, address, password string) error
	// DeleteMailbox returns the number of messages that were removed.
	DeleteMailbox(ctx context.Context, address string) (deletedMessages int64, err error)
	Usage(ctx context.Context, address string) (usedBytes int64, err error)
	// SetForwarder registers (or replaces) a multi-target alias ("info@x.com").
	// For a catch-all ("@x.com") exactly one target is supported by Stalwart.
	SetForwarder(ctx context.Context, address string, targets []string) error
	DeleteForwarder(ctx context.Context, address string) error
}

type Client struct {
	base string
	user string
	pass string
	hc   *http.Client
}

func New(baseURL, user, pass string, timeout time.Duration) *Client {
	return &Client{base: strings.TrimRight(baseURL, "/"), user: user, pass: pass, hc: &http.Client{Timeout: timeout}}
}

const (
	capCore  = "urn:ietf:params:jmap:core"
	capSW    = "urn:stalwart:jmap"
	capMail  = "urn:ietf:params:jmap:mail"
	capQuota = "urn:ietf:params:jmap:quota"
)

type call [3]any // [name, args, id]

type response struct {
	MethodResponses [][3]json.RawMessage `json:"methodResponses"`
}

type methodError struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

// invoke sends a single method call and decodes its arguments into out.
func (c *Client) invoke(ctx context.Context, method string, args map[string]any, out any) error {
	body, _ := json.Marshal(map[string]any{
		"using":       []string{capCore, capSW, capMail, capQuota},
		"methodCalls": []call{{method, args, "c1"}},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/jmap/", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.user != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	resp.Body.Close()
	switch {
	case resp.StatusCode >= 500:
		return fmt.Errorf("%w: status %d", ErrUnavailable, resp.StatusCode)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("stalwart rejected credentials (status %d)", resp.StatusCode)
	case resp.StatusCode >= 400:
		return fmt.Errorf("stalwart: status %d: %s", resp.StatusCode, truncate(string(raw)))
	}
	var r response
	if err := json.Unmarshal(raw, &r); err != nil || len(r.MethodResponses) == 0 {
		return fmt.Errorf("stalwart: unexpected response: %s", truncate(string(raw)))
	}
	var name string
	_ = json.Unmarshal(r.MethodResponses[0][0], &name)
	if name == "error" {
		var me methodError
		_ = json.Unmarshal(r.MethodResponses[0][1], &me)
		return fmt.Errorf("stalwart error %s: %s", me.Type, me.Description)
	}
	if out != nil {
		return json.Unmarshal(r.MethodResponses[0][1], out)
	}
	return nil
}

func truncate(s string) string {
	if len(s) > 300 {
		return s[:300]
	}
	return s
}

func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/jmap/session", nil)
	if err != nil {
		return err
	}
	if c.user != "" {
		req.SetBasicAuth(c.user, c.pass)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("%w: status %d", ErrUnavailable, resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("stalwart session check failed (status %d)", resp.StatusCode)
	}
	return nil
}

// ---- generic helpers ----

type setResult struct {
	Created map[string]struct {
		ID string `json:"id"`
	} `json:"created"`
	NotCreated   map[string]setError `json:"notCreated"`
	NotUpdated   map[string]setError `json:"notUpdated"`
	NotDestroyed map[string]setError `json:"notDestroyed"`
}

type setError struct {
	Type        string   `json:"type"`
	Description string   `json:"description"`
	Properties  []string `json:"properties"`
}

func (e setError) err() error {
	switch e.Type {
	case "primaryKeyViolation", "alreadyExists":
		return ErrConflict
	case "notFound":
		return ErrNotFound
	}
	return fmt.Errorf("stalwart rejected change: %s %s %v", e.Type, e.Description, e.Properties)
}

func (c *Client) create(ctx context.Context, object string, fields map[string]any) (string, error) {
	var r setResult
	if err := c.invoke(ctx, "x:"+object+"/set", map[string]any{"create": map[string]any{"n": fields}}, &r); err != nil {
		return "", err
	}
	if e, ok := r.NotCreated["n"]; ok {
		return "", e.err()
	}
	if created, ok := r.Created["n"]; ok {
		return created.ID, nil
	}
	return "", errors.New("stalwart: create returned no id")
}

func (c *Client) update(ctx context.Context, object, id string, fields map[string]any) error {
	var r setResult
	if err := c.invoke(ctx, "x:"+object+"/set", map[string]any{"update": map[string]any{id: fields}}, &r); err != nil {
		return err
	}
	if e, ok := r.NotUpdated[id]; ok {
		return e.err()
	}
	return nil
}

func (c *Client) destroy(ctx context.Context, object, id string) error {
	var r setResult
	if err := c.invoke(ctx, "x:"+object+"/set", map[string]any{"destroy": []string{id}}, &r); err != nil {
		return err
	}
	if e, ok := r.NotDestroyed[id]; ok {
		return e.err()
	}
	return nil
}

type queryResult struct {
	IDs []string `json:"ids"`
}

type getResult[T any] struct {
	List []T `json:"list"`
}

type domainObj struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DNSZoneFile string `json:"dnsZoneFile"`
}

func (c *Client) findDomain(ctx context.Context, name string) (*domainObj, error) {
	var q queryResult
	if err := c.invoke(ctx, "x:Domain/query", map[string]any{"filter": map[string]any{"name": name}}, &q); err != nil {
		return nil, err
	}
	if len(q.IDs) > 0 {
		var g getResult[domainObj]
		if err := c.invoke(ctx, "x:Domain/get", map[string]any{"ids": q.IDs, "properties": []string{"name", "dnsZoneFile"}}, &g); err != nil {
			return nil, err
		}
		for _, d := range g.List {
			if strings.EqualFold(d.Name, name) { // never trust the filter alone
				return &d, nil
			}
		}
	}
	return nil, ErrNotFound
}

type accountObj struct {
	ID           string `json:"id"`
	EmailAddress string `json:"emailAddress"`
	UsedDisk     int64  `json:"usedDiskQuota"`
}

func splitAddr(address string) (local, domain string, err error) {
	i := strings.LastIndex(address, "@")
	if i <= 0 {
		return "", "", fmt.Errorf("invalid address %q", address)
	}
	return address[:i], address[i+1:], nil
}

func (c *Client) findAccount(ctx context.Context, address string) (*accountObj, error) {
	local, dom, err := splitAddr(address)
	if err != nil {
		return nil, err
	}
	d, err := c.findDomain(ctx, dom)
	if err != nil {
		return nil, err
	}
	var q queryResult
	if err := c.invoke(ctx, "x:Account/query", map[string]any{"filter": map[string]any{"name": local, "domainId": d.ID}}, &q); err != nil {
		return nil, err
	}
	if len(q.IDs) == 0 {
		return nil, ErrNotFound
	}
	var g getResult[accountObj]
	if err := c.invoke(ctx, "x:Account/get", map[string]any{"ids": q.IDs, "properties": []string{"emailAddress", "usedDiskQuota"}}, &g); err != nil {
		return nil, err
	}
	for _, a := range g.List {
		if strings.EqualFold(a.EmailAddress, address) { // verify, don't trust the filter
			return &a, nil
		}
	}
	return nil, ErrNotFound
}

// ---- domains ----

func (c *Client) EnsureDomain(ctx context.Context, name string) (string, error) {
	if d, err := c.findDomain(ctx, name); err == nil {
		return d.ID, nil // idempotent (FR-14)
	} else if !errors.Is(err, ErrNotFound) {
		return "", err
	}
	id, err := c.create(ctx, "Domain", map[string]any{"name": name})
	if errors.Is(err, ErrConflict) { // lost a race: it exists now
		if d, ferr := c.findDomain(ctx, name); ferr == nil {
			return d.ID, nil
		}
	}
	return id, err
}

func (c *Client) DeleteDomain(ctx context.Context, name string) error {
	d, err := c.findDomain(ctx, name)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	// DKIM signatures are linked to the domain and must be removed first.
	var sigs getResult[struct {
		ID       string `json:"id"`
		DomainID string `json:"domainId"`
	}]
	if err := c.invoke(ctx, "x:DkimSignature/get", map[string]any{"ids": nil, "properties": []string{"domainId"}}, &sigs); err != nil {
		return err
	}
	for _, sg := range sigs.List {
		if sg.DomainID == d.ID {
			if err := c.destroy(ctx, "DkimSignature", sg.ID); err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
		}
	}
	return c.destroy(ctx, "Domain", d.ID)
}

// DomainDKIM reads the RSA DKIM key from the domain's zone file. Stalwart
// generates keys shortly after the domain is created, so poll briefly.
func (c *Client) DomainDKIM(ctx context.Context, domain string) (string, string, error) {
	var lastErr error
	for i := 0; i < 20; i++ {
		d, err := c.findDomain(ctx, domain)
		if err != nil {
			return "", "", err
		}
		sel, pub, perr := ParseDKIM(d.DNSZoneFile)
		if perr == nil {
			return sel, pub, nil
		}
		lastErr = perr
		select {
		case <-ctx.Done():
			return "", "", fmt.Errorf("%w: %v", ErrUnavailable, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
	return "", "", lastErr
}

var quotedRe = regexp.MustCompile(`"([^"]*)"`)

// ParseDKIM extracts (selector, p=) of the rsa record from a zone file.
func ParseDKIM(zone string) (string, string, error) {
	lines := strings.Split(zone, "\n")
	for i := 0; i < len(lines); i++ {
		l := strings.TrimSpace(lines[i])
		if !strings.Contains(l, "._domainkey.") || !strings.Contains(l, " TXT ") {
			continue
		}
		rec := l
		if strings.HasSuffix(l, "(") { // multi-line record
			for i++; i < len(lines); i++ {
				t := strings.TrimSpace(lines[i])
				if t == ")" {
					break
				}
				rec += " " + t
			}
		}
		name := strings.Fields(rec)[0]
		var joined strings.Builder
		for _, m := range quotedRe.FindAllStringSubmatch(rec, -1) {
			joined.WriteString(m[1])
		}
		txt := joined.String()
		if !strings.Contains(strings.ToLower(txt), "k=rsa") {
			continue
		}
		for _, kv := range strings.Split(txt, ";") {
			kv = strings.TrimSpace(kv)
			if strings.HasPrefix(kv, "p=") {
				selector := strings.SplitN(name, "._domainkey.", 2)[0]
				return selector, strings.Join(strings.Fields(kv[2:]), ""), nil
			}
		}
	}
	return "", "", errors.New("stalwart returned no RSA DKIM key for the domain")
}

// ---- mailboxes ----

func (c *Client) CreateMailbox(ctx context.Context, address, password string, quota int64) (string, error) {
	local, dom, err := splitAddr(address)
	if err != nil {
		return "", err
	}
	d, err := c.findDomain(ctx, dom)
	if err != nil {
		return "", err
	}
	return c.create(ctx, "Account", map[string]any{
		"@type": "User", "name": local, "domainId": d.ID,
		"credentials": map[string]any{"0": map[string]any{"@type": "Password", "secret": password}},
		"quotas":      map[string]any{"maxDiskQuota": quota},
	})
}

func (c *Client) SetQuota(ctx context.Context, address string, quota int64) error {
	a, err := c.findAccount(ctx, address)
	if err != nil {
		return err
	}
	return c.update(ctx, "Account", a.ID, map[string]any{"quotas": map[string]any{"maxDiskQuota": quota}})
}

func (c *Client) SetPassword(ctx context.Context, address, password string) error {
	a, err := c.findAccount(ctx, address)
	if err != nil {
		return err
	}
	return c.update(ctx, "Account", a.ID, map[string]any{
		"credentials": map[string]any{"0": map[string]any{"@type": "Password", "secret": password}},
	})
}

func (c *Client) DeleteMailbox(ctx context.Context, address string) (int64, error) {
	a, err := c.findAccount(ctx, address)
	if errors.Is(err, ErrNotFound) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var q struct {
		Total int64 `json:"total"`
	}
	var count int64
	if err := c.invoke(ctx, "Email/query", map[string]any{"accountId": a.ID, "calculateTotal": true, "limit": 0}, &q); err == nil {
		count = q.Total
	}
	if err := c.destroy(ctx, "Account", a.ID); err != nil {
		return 0, err
	}
	return count, nil
}

func (c *Client) Usage(ctx context.Context, address string) (int64, error) {
	a, err := c.findAccount(ctx, address)
	if err != nil {
		return 0, err
	}
	return a.UsedDisk, nil
}

// ---- forwarders ----

type listObj struct {
	ID           string `json:"id"`
	EmailAddress string `json:"emailAddress"`
}

func (c *Client) findList(ctx context.Context, address string) (*listObj, error) {
	// MailingList/query has no name filter, so list and match on the address.
	var g getResult[listObj]
	if err := c.invoke(ctx, "x:MailingList/get", map[string]any{"ids": nil, "properties": []string{"emailAddress"}}, &g); err != nil {
		return nil, err
	}
	for _, l := range g.List {
		if strings.EqualFold(l.EmailAddress, address) {
			return &l, nil
		}
	}
	return nil, ErrNotFound
}

func (c *Client) SetForwarder(ctx context.Context, address string, targets []string) error {
	if strings.HasPrefix(address, "@") { // catch-all lives on the domain object
		if len(targets) != 1 {
			return errors.New("stalwart supports exactly one catch-all target")
		}
		d, err := c.findDomain(ctx, strings.TrimPrefix(address, "@"))
		if err != nil {
			return err
		}
		return c.update(ctx, "Domain", d.ID, map[string]any{"catchAllAddress": targets[0]})
	}
	local, dom, err := splitAddr(address)
	if err != nil {
		return err
	}
	d, err := c.findDomain(ctx, dom)
	if err != nil {
		return err
	}
	recipients := map[string]any{}
	for _, t := range targets {
		recipients[t] = true
	}
	if l, err := c.findList(ctx, address); err == nil {
		return c.update(ctx, "MailingList", l.ID, map[string]any{"recipients": recipients})
	} else if !errors.Is(err, ErrNotFound) {
		return err
	}
	_, err = c.create(ctx, "MailingList", map[string]any{"name": local, "domainId": d.ID, "recipients": recipients})
	return err
}

func (c *Client) DeleteForwarder(ctx context.Context, address string) error {
	if strings.HasPrefix(address, "@") {
		d, err := c.findDomain(ctx, strings.TrimPrefix(address, "@"))
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		return c.update(ctx, "Domain", d.ID, map[string]any{"catchAllAddress": nil})
	}
	l, err := c.findList(ctx, address)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return c.destroy(ctx, "MailingList", l.ID)
}
