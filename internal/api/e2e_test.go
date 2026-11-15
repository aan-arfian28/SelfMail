package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"selfmail/internal/api"
	"selfmail/internal/cloudflare"
	"selfmail/internal/config"
	"selfmail/internal/db"
	"selfmail/internal/dnscheck"
	"selfmail/internal/secret"
	"selfmail/internal/service"
	"selfmail/internal/stalwart"
	"selfmail/internal/store"
	"selfmail/internal/worker"
	"selfmail/migrations"
)

// ---- fakes ----

type fakeSW struct {
	mu        sync.Mutex
	down      bool
	domains   map[string]bool
	mailboxes map[string]int64 // address -> used bytes
	quotas    map[string]int64
	passwords map[string]string
	fwd       map[string][]string
}

func newFakeSW() *fakeSW {
	return &fakeSW{domains: map[string]bool{}, mailboxes: map[string]int64{}, quotas: map[string]int64{}, passwords: map[string]string{}, fwd: map[string][]string{}}
}
func (f *fakeSW) chk() error {
	if f.down {
		return stalwart.ErrUnavailable
	}
	return nil
}
func (f *fakeSW) Ping(context.Context) error { f.mu.Lock(); defer f.mu.Unlock(); return f.chk() }
func (f *fakeSW) EnsureDomain(_ context.Context, n string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.chk(); err != nil {
		return "", err
	}
	f.domains[n] = true
	return n, nil
}
func (f *fakeSW) DeleteDomain(_ context.Context, n string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.chk(); err != nil {
		return err
	}
	delete(f.domains, n)
	return nil
}
func (f *fakeSW) DomainDKIM(_ context.Context, d string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return "sel1", "PUBKEY" + d, f.chk()
}
func (f *fakeSW) CreateMailbox(_ context.Context, a, pw string, q int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.chk(); err != nil {
		return "", err
	}
	f.mailboxes[a], f.quotas[a], f.passwords[a] = 0, q, pw
	return a, nil
}
func (f *fakeSW) SetQuota(_ context.Context, a string, q int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.quotas[a] = q
	return f.chk()
}
func (f *fakeSW) SetPassword(_ context.Context, a, pw string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.passwords[a] = pw
	return f.chk()
}
func (f *fakeSW) DeleteMailbox(_ context.Context, a string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.chk(); err != nil {
		return 0, err
	}
	delete(f.mailboxes, a)
	return 42, nil
}
func (f *fakeSW) Usage(_ context.Context, a string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.chk(); err != nil {
		return 0, err
	}
	return f.mailboxes[a], nil
}
func (f *fakeSW) SetForwarder(_ context.Context, a string, t []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fwd[a] = t
	return f.chk()
}
func (f *fakeSW) DeleteForwarder(_ context.Context, a string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.fwd, a)
	return f.chk()
}

type fakeDNS struct {
	mu  sync.Mutex
	mx  map[string][]*net.MX
	txt map[string][]string
	err error
}

func (f *fakeDNS) LookupMX(_ context.Context, n string) ([]*net.MX, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if v, ok := f.mx[n]; ok {
		return v, nil
	}
	return nil, &net.DNSError{IsNotFound: true}
}
func (f *fakeDNS) LookupTXT(_ context.Context, n string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	if v, ok := f.txt[n]; ok {
		return v, nil
	}
	return nil, &net.DNSError{IsNotFound: true}
}

type fakeCF struct {
	mu       sync.Mutex
	records  map[string]string // id -> type|name|content
	n        int
	conflict bool
}

func (f *fakeCF) Upsert(_ context.Context, _ string, r cloudflare.Record, known string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.conflict && r.Type == "MX" {
		return "", cloudflare.ErrConflict
	}
	key := r.Type + "|" + r.Name + "|" + r.Content
	if known != "" {
		f.records[known] = key
		return known, nil
	}
	for id, v := range f.records {
		if v == key {
			return id, nil
		}
	}
	f.n++
	id := fmt.Sprintf("cf%d", f.n)
	f.records[id] = key
	return id, nil
}
func (f *fakeCF) Delete(_ context.Context, _, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.records, id)
	return nil
}

// ---- harness ----

type env struct {
	t     *testing.T
	srv   *httptest.Server
	sw    *fakeSW
	dns   *fakeDNS
	cf    *fakeCF
	st    *store.Store
	svc   *service.Service
	token string
	cfg   *config.Config
}

func setup(t *testing.T) *env {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := db.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx, pool, migrations.FS); err != nil {
		t.Fatal(err)
	}
	st := store.New(pool)
	tok, hash, _ := secret.NewToken()
	if err := st.CreateCredential(ctx, "test-token", hash); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DATABASE_URL", url)
	t.Setenv("STALWART_URL", "http://stalwart.invalid")
	t.Setenv("MAIL_HOST", "mail.example.com")
	t.Setenv("SERVER_IP", "203.0.113.5")
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, sw: newFakeSW(), dns: &fakeDNS{mx: map[string][]*net.MX{}, txt: map[string][]string{}}, cf: &fakeCF{records: map[string]string{}}, st: st, token: tok, cfg: cfg}
	log := slog.New(slog.NewTextHandler(testLogWriter{t}, nil))
	e.svc = &service.Service{Cfg: cfg, Store: st, Log: log, SW: e.sw, CF: e.cf,
		DNS: &dnscheck.Checker{R: e.dns, Timeout: time.Second}}
	e.srv = httptest.NewServer(api.New(e.svc, st, cfg, log).Handler())
	t.Cleanup(e.srv.Close)
	return e
}

func (e *env) do(method, path string, body any, auth bool) (int, map[string]any, http.Header) {
	e.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rdr)
	if auth {
		req.Header.Set("Authorization", "Bearer "+e.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return resp.StatusCode, m, resp.Header
}

func (e *env) expect(code int, got int, m map[string]any) {
	e.t.Helper()
	if got != code {
		e.t.Fatalf("expected %d got %d: %v", code, got, m)
	}
}

func errCode(m map[string]any) string {
	if x, ok := m["error"].(map[string]any); ok {
		s, _ := x["code"].(string)
		return s
	}
	return ""
}

func (e *env) publishDNS(domain string) {
	e.dns.mu.Lock()
	defer e.dns.mu.Unlock()
	e.dns.mx[domain] = []*net.MX{{Host: "mail.example.com.", Pref: 10}}
	e.dns.txt[domain] = []string{"v=spf1 ip4:203.0.113.5 -all"}
	e.dns.txt["sel1._domainkey."+domain] = []string{"v=DKIM1; k=rsa; p=PUBKEY" + domain}
	e.dns.txt["_dmarc."+domain] = []string{"v=DMARC1; p=quarantine"}
}

// ---- tests ----

func TestAuth(t *testing.T) {
	e := setup(t)
	code, m, _ := e.do("GET", "/domains", nil, false)
	e.expect(401, code, m)
	req, _ := http.NewRequest("GET", e.srv.URL+"/domains", nil)
	req.Header.Set("Authorization", "Bearer smk_wrong")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Fatal(resp.StatusCode)
	}
	code, m, _ = e.do("GET", "/domains", nil, true)
	e.expect(200, code, m)
	var used *time.Time
	e.st.P.QueryRow(context.Background(), `select last_used_at from api_credentials`).Scan(&used)
	if used == nil {
		t.Fatal("last_used_at not updated")
	}
}

func TestDomainLifecycleAndHonestDNS(t *testing.T) {
	e := setup(t)
	code, m, _ := e.do("POST", "/domains", map[string]any{"name": "Example.com"}, true)
	e.expect(201, code, m)
	if m["dns_status"] != "pending" {
		t.Fatalf("dns_status = %v", m["dns_status"])
	}
	if recs := m["dns_records"].([]any); len(recs) != 4 {
		t.Fatalf("dns_records = %d", len(recs))
	}
	if pr := m["pending_records"].([]any); len(pr) != 4 {
		t.Fatalf("pending_records = %d", len(pr))
	}
	var jobs int
	e.st.P.QueryRow(context.Background(), `select count(*) from jobs where job_type='provision_dns_records' and status='queued'`).Scan(&jobs)
	if jobs != 1 {
		t.Fatalf("jobs = %d", jobs)
	}

	code, m, _ = e.do("POST", "/domains", map[string]any{"name": "example.com"}, true)
	e.expect(409, code, m)

	// nothing published -> not verified
	code, m, _ = e.do("GET", "/domains/example.com/status", nil, true)
	e.expect(200, code, m)
	if m["dns_status"] != "pending" || m["checked_at"] == nil {
		t.Fatalf("%v", m)
	}

	// a stale old MX is a mismatch, never silently overwritten or "ok"
	e.dns.mx["example.com"] = []*net.MX{{Host: "old.other.net.", Pref: 5}}
	code, m, _ = e.do("POST", "/domains/example.com/verify", nil, true)
	e.expect(200, code, m)
	if m["dns_status"] != "failed" {
		t.Fatalf("expected failed on mismatch, got %v", m["dns_status"])
	}

	// resolver down -> fail closed
	e.dns.err = fmt.Errorf("timeout")
	code, m, _ = e.do("POST", "/domains/example.com/verify", nil, true)
	e.expect(200, code, m)
	if m["dns_status"] == "verified" {
		t.Fatal("verified while resolver down")
	}
	e.dns.err = nil

	e.publishDNS("example.com")
	code, m, _ = e.do("POST", "/domains/example.com/verify", nil, true)
	e.expect(200, code, m)
	if m["dns_status"] != "verified" || len(m["pending_records"].([]any)) != 0 {
		t.Fatalf("%v", m)
	}

	code, m, _ = e.do("GET", "/domains/example.com/dns-records", nil, true)
	e.expect(200, code, m)
	recs := m["records"].([]any)
	if !strings.Contains(recs[2].(map[string]any)["expected"].(string), "PUBKEYexample.com") {
		t.Fatalf("dkim expected must carry the real key: %v", recs[2])
	}

	code, m, _ = e.do("GET", "/domains/nope.com", nil, true)
	e.expect(404, code, m)
	code, m, _ = e.do("POST", "/domains", map[string]any{"name": "not a domain"}, true)
	e.expect(400, code, m)
}

func TestIDNDomain(t *testing.T) {
	e := setup(t)
	code, m, _ := e.do("POST", "/domains", map[string]any{"name": "éxample.com"}, true)
	e.expect(201, code, m)
	if m["punycode_name"] != "xn--xample-9ua.com" {
		t.Fatalf("punycode = %v", m["punycode_name"])
	}
}

func TestMailboxFlow(t *testing.T) {
	e := setup(t)
	e.do("POST", "/domains", map[string]any{"name": "example.com"}, true)
	e.publishDNS("example.com")

	code, m, _ := e.do("POST", "/domains/example.com/mailboxes", map[string]any{"local_part": "halo", "quota_bytes": 1000}, true)
	e.expect(201, code, m)
	pw, _ := m["initial_password"].(string)
	if len(pw) != 16 {
		t.Fatalf("password %q", pw)
	}
	if m["address"] != "halo@example.com" || m["used_bytes"].(float64) != 0 || m["dns_status"] == nil {
		t.Fatalf("%v", m)
	}

	code, m, _ = e.do("POST", "/domains/example.com/mailboxes", map[string]any{"local_part": "halo", "quota_bytes": 1000}, true)
	e.expect(409, code, m)
	code, m, _ = e.do("POST", "/domains/example.com/mailboxes", map[string]any{"local_part": "x", "quota_bytes": 0}, true)
	e.expect(400, code, m)
	code, m, _ = e.do("POST", "/domains/example.com/mailboxes", map[string]any{"local_part": "weak", "quota_bytes": 10, "password": "short"}, true)
	e.expect(400, code, m)
	code, m, _ = e.do("POST", "/domains/example.com/mailboxes", map[string]any{"local_part": "chosen", "quota_bytes": 10, "password": "Str0ng-Password!"}, true)
	e.expect(201, code, m)
	if _, has := m["initial_password"]; has {
		t.Fatal("supplied password must not be echoed")
	}

	// quota below usage -> 409
	e.sw.mailboxes["halo@example.com"] = 800
	time.Sleep(10 * time.Millisecond)
	code, m, _ = e.do("PATCH", "/mailboxes/halo@example.com", map[string]any{"quota_bytes": 500}, true)
	e.expect(409, code, m)
	if errCode(m) != "quota_below_usage" {
		t.Fatalf("%v", m)
	}
	code, m, _ = e.do("PATCH", "/mailboxes/halo@example.com", map[string]any{"quota_bytes": 900}, true)
	e.expect(200, code, m)
	if m["quota_bytes"].(float64) != 900 || m["percent_used"].(float64) != 88.89 || m["over_quota"] != false {
		t.Fatalf("%v", m)
	}

	// dns_status consistent with domain status
	_, dm, _ := e.do("GET", "/domains/example.com/status", nil, true)
	_, mm, _ := e.do("GET", "/mailboxes/halo@example.com", nil, true)
	if dm["dns_status"] != mm["dns_status"] {
		t.Fatalf("inconsistent dns_status %v vs %v", dm["dns_status"], mm["dns_status"])
	}

	// password reset: returned once, never in audit logs / DB
	code, m, _ = e.do("POST", "/mailboxes/halo@example.com/password/reset", nil, true)
	e.expect(200, code, m)
	newPw := m["new_password"].(string)
	var leaked int
	e.st.P.QueryRow(context.Background(), `select count(*) from audit_logs where metadata::text like '%'||$1||'%' or metadata::text like '%'||$2||'%' or metadata::text ilike '%password%'`, newPw, pw).Scan(&leaked)
	if leaked != 0 {
		t.Fatal("password found in audit logs")
	}
	code, m, _ = e.do("POST", "/mailboxes/ghost@example.com/password/reset", nil, true)
	e.expect(404, code, m)

	// Stalwart down: 503 + retry_after, and no phantom mailbox row is left behind
	e.sw.down = true
	code, m, h := e.do("POST", "/domains/example.com/mailboxes", map[string]any{"local_part": "late", "quota_bytes": 10}, true)
	e.expect(503, code, m)
	if h.Get("Retry-After") == "" || m["error"].(map[string]any)["retry_after"].(float64) <= 0 {
		t.Fatalf("missing retry_after %v", m)
	}
	// within the 60s cache window fresh data may be served; once the cache is
	// stale and Stalwart is unreachable we must fail closed (503), never 0/stale.
	code, m, _ = e.do("GET", "/usage", nil, true)
	e.expect(200, code, m)
	e.st.P.Exec(context.Background(), `update mailboxes set usage_measured_at = now() - interval '10 minutes'`)
	code, m, _ = e.do("GET", "/usage", nil, true)
	e.expect(503, code, m)
	code, m, _ = e.do("GET", "/mailboxes/halo@example.com", nil, true)
	e.expect(503, code, m)
	e.sw.down = false
	var n int
	e.st.P.QueryRow(context.Background(), `select count(*) from mailboxes where local_part='late'`).Scan(&n)
	if n != 0 {
		t.Fatal("phantom row after failed provisioning")
	}

	// delete -> count
	code, m, _ = e.do("DELETE", "/mailboxes/chosen@example.com", nil, true)
	e.expect(200, code, m)
	if m["deleted_messages_count"].(float64) != 42 {
		t.Fatalf("%v", m)
	}
	code, m, _ = e.do("GET", "/mailboxes/chosen@example.com", nil, true)
	e.expect(404, code, m)
}

func TestAliasesAndCatchAll(t *testing.T) {
	e := setup(t)
	e.do("POST", "/domains", map[string]any{"name": "example.com"}, true)
	e.do("POST", "/domains/example.com/mailboxes", map[string]any{"local_part": "halo", "quota_bytes": 1000}, true)

	code, m, _ := e.do("POST", "/domains/example.com/aliases", map[string]any{"local_part": "info", "targets": []string{"ghost@example.com"}}, true)
	e.expect(400, code, m)
	if m["error"].(map[string]any)["details"] == nil {
		t.Fatal("expected invalid target list")
	}
	code, m, _ = e.do("POST", "/domains/example.com/aliases", map[string]any{"local_part": "info", "targets": []string{}}, true)
	e.expect(400, code, m)
	six := []string{"a@example.com", "b@example.com", "c@example.com", "d@example.com", "e@example.com", "f@example.com"}
	code, m, _ = e.do("POST", "/domains/example.com/aliases", map[string]any{"local_part": "info", "targets": six}, true)
	e.expect(400, code, m)
	code, m, _ = e.do("POST", "/domains/example.com/aliases", map[string]any{"local_part": "halo", "targets": []string{"halo@example.com"}}, true)
	e.expect(409, code, m)
	code, m, _ = e.do("POST", "/domains/example.com/aliases", map[string]any{"local_part": "info", "targets": []string{"halo@example.com"}}, true)
	e.expect(201, code, m)
	// alias -> alias is rejected
	code, m, _ = e.do("POST", "/domains/example.com/aliases", map[string]any{"local_part": "loop", "targets": []string{"info@example.com"}}, true)
	e.expect(400, code, m)
	code, m, _ = e.do("POST", "/domains/example.com/aliases", map[string]any{"local_part": "info", "targets": []string{"halo@example.com"}}, true)
	e.expect(409, code, m)

	// catch-all
	code, m, _ = e.do("PATCH", "/domains/example.com", map[string]any{"catch_all": true}, true)
	e.expect(400, code, m)
	code, m, _ = e.do("PATCH", "/domains/example.com", map[string]any{"catch_all": true, "catch_all_target": "halo@example.com"}, true)
	e.expect(200, code, m)
	if m["catch_all"] != true || m["catch_all_target"] != "halo@example.com" {
		t.Fatalf("%v", m)
	}
	if len(e.sw.fwd["@example.com"]) != 1 {
		t.Fatal("catch-all not synced to stalwart")
	}

	// mailbox in use cannot be deleted
	code, m, _ = e.do("DELETE", "/mailboxes/halo@example.com", nil, true)
	e.expect(409, code, m)

	code, _, _ = e.do("DELETE", "/domains/example.com/aliases/info", nil, true)
	if code != 204 {
		t.Fatal(code)
	}
	code, m, _ = e.do("DELETE", "/domains/example.com/aliases/info", nil, true)
	e.expect(404, code, m)

	// domain delete: blocked by mailbox, force clears everything
	code, m, _ = e.do("DELETE", "/domains/example.com", nil, true)
	e.expect(409, code, m)
	code, _, _ = e.do("DELETE", "/domains/example.com?force=true", nil, true)
	if code != 204 {
		t.Fatalf("force delete = %d", code)
	}
	var n int
	e.st.P.QueryRow(context.Background(), `select (select count(*) from domains)+(select count(*) from mailboxes)`).Scan(&n)
	if n != 0 {
		t.Fatalf("rows left: %d", n)
	}
	if len(e.sw.fwd) != 0 || len(e.sw.mailboxes) != 0 || len(e.sw.domains) != 0 {
		t.Fatal("stalwart state not cleaned")
	}
}

func TestWorkerDNSProvisioningIsIdempotent(t *testing.T) {
	e := setup(t)
	e.do("POST", "/domains", map[string]any{"name": "example.com"}, true)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := worker.New(e.svc, e.st, e.cfg, log)
	_ = w
	ctx := context.Background()
	run := func() {
		j, err := e.st.ClaimJob(ctx, "t")
		if err != nil || j == nil {
			t.Fatalf("claim: %v %v", j, err)
		}
		if err := e.svc.RunJob(ctx, j); err != nil {
			t.Fatal(err)
		}
		e.st.CompleteJob(ctx, j.ID)
	}
	run()
	if len(e.cf.records) != 4 {
		t.Fatalf("records = %d", len(e.cf.records))
	}
	// re-run the same job twice more -> still 4
	d, _ := e.st.GetDomain(ctx, "example.com")
	for i := 0; i < 2; i++ {
		e.st.Enqueue(ctx, "provision_dns_records", &d.ID, nil, nil, time.Now())
		run()
	}
	if len(e.cf.records) != 4 {
		t.Fatalf("duplicates created: %d", len(e.cf.records))
	}
	var st string
	e.st.P.QueryRow(ctx, `select dns_status from domains`).Scan(&st)
	if st != "propagating" {
		t.Fatalf("status after provisioning = %s", st)
	}
}

func TestJobRetryBackoffAndPermanentFailure(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	id, _ := e.st.Enqueue(ctx, "verify_dns", nil, nil, nil, time.Now())
	for attempt := 1; attempt <= 5; attempt++ {
		j, err := e.st.ClaimJob(ctx, "w")
		if err != nil || j == nil {
			t.Fatalf("attempt %d: %v %v", attempt, j, err)
		}
		if j.Attempts != attempt {
			t.Fatalf("attempts = %d", j.Attempts)
		}
		e.st.FailJob(ctx, j, fmt.Errorf("boom"), time.Now().Add(-time.Second))
	}
	var status, lastErr string
	e.st.P.QueryRow(ctx, `select status,last_error from jobs where id=$1`, id).Scan(&status, &lastErr)
	if status != "failed" || lastErr != "boom" {
		t.Fatalf("%s %s", status, lastErr)
	}
	// future run_at is not claimable; locked jobs are not double-claimed
	e.st.Enqueue(ctx, "verify_dns", nil, nil, nil, time.Now().Add(time.Hour))
	if j, _ := e.st.ClaimJob(ctx, "w"); j != nil {
		t.Fatal("claimed future job")
	}
	e.st.Enqueue(ctx, "verify_dns", nil, nil, nil, time.Now())
	a, _ := e.st.ClaimJob(ctx, "w1")
	b, _ := e.st.ClaimJob(ctx, "w2")
	if a == nil || b != nil {
		t.Fatal("double claim")
	}
	// stuck job recovery
	e.st.P.Exec(ctx, `update jobs set locked_at = now() - interval '1 hour' where id=$1`, a.ID)
	if n, _ := e.st.RecoverStuckJobs(ctx, 5*time.Minute); n != 1 {
		t.Fatalf("recovered %d", n)
	}
}

func TestReconcileCleansSoftDeleted(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	e.do("POST", "/domains", map[string]any{"name": "example.com"}, true)
	e.do("POST", "/domains/example.com/mailboxes", map[string]any{"local_part": "halo", "quota_bytes": 1000}, true)
	// Stalwart is down while deleting: API answers 202 and keeps the row soft-deleted
	e.sw.down = true
	code, m, _ := e.do("DELETE", "/mailboxes/halo@example.com", nil, true)
	e.expect(202, code, m)
	e.sw.down = false
	if _, ok := e.sw.mailboxes["halo@example.com"]; !ok {
		t.Fatal("test setup: mailbox should still exist in stalwart")
	}
	if err := e.svc.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.sw.mailboxes["halo@example.com"]; ok {
		t.Fatal("reconcile did not delete in stalwart")
	}
	var n int
	e.st.P.QueryRow(ctx, `select count(*) from mailboxes`).Scan(&n)
	if n != 0 {
		t.Fatal("row not hard-deleted")
	}
}

func TestAuditAndHealthAndAutoconfig(t *testing.T) {
	e := setup(t)
	e.do("POST", "/domains", map[string]any{"name": "example.com"}, true)
	var actor string
	var cnt int
	e.st.P.QueryRow(context.Background(), `select actor, count(*) from audit_logs where action='create_domain' group by actor`).Scan(&actor, &cnt)
	if actor != "test-token" || cnt != 1 {
		t.Fatalf("audit: %s %d", actor, cnt)
	}

	code, m, _ := e.do("GET", "/readyz", nil, false)
	e.expect(200, code, m)
	e.sw.down = true
	code, m, _ = e.do("GET", "/readyz", nil, false)
	e.expect(503, code, m)
	e.sw.down = false

	resp, err := http.Get(e.srv.URL + "/mail/config-v1.1.xml?emailaddress=halo@example.com")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/xml") || !strings.Contains(string(b), "<port>993</port>") {
		t.Fatalf("%d %s %s", resp.StatusCode, resp.Header.Get("Content-Type"), b)
	}
	resp, _ = http.Get(e.srv.URL + "/.well-known/autoconfig/mail/config-v1.1.xml?emailaddress=x@unknown.org")
	if resp.StatusCode != 404 {
		t.Fatal(resp.StatusCode)
	}
}

type testLogWriter struct{ t *testing.T }

func (w testLogWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}
