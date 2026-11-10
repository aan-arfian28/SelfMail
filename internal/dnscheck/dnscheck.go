// Package dnscheck performs honest, fail-closed DNS verification (FR-02).
// It always queries public resolvers directly (no local cache) and never
// reports "ok" when a query failed.
package dnscheck

import (
	"context"
	"errors"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	StatusOK       = "ok"
	StatusMismatch = "mismatch"
	StatusMissing  = "missing"
	StatusError    = "error"

	DomainPending     = "pending"
	DomainPropagating = "propagating"
	DomainVerified    = "verified"
	DomainFailed      = "failed"
	DomainError       = "error"
)

// Expected describes one DNS record the domain must publish.
type Expected struct {
	Purpose string `json:"purpose"` // mx|spf|dkim|dmarc
	Type    string `json:"type"`    // MX|TXT
	Name    string `json:"name"`    // fully qualified (punycode) name
	Value   string `json:"expected"`
}

// Result is the verification outcome for one record.
type Result struct {
	Expected
	Detected string `json:"detected"`
	Status   string `json:"status"`
	Error    string `json:"error,omitempty"`
}

// Lookuper is the subset of net.Resolver used; it lets tests inject fakes.
type Lookuper interface {
	LookupMX(ctx context.Context, name string) ([]*net.MX, error)
	LookupTXT(ctx context.Context, name string) ([]string, error)
}

// NewPublicResolver returns a Lookuper that queries the given servers
// ("host:port") in order, falling back only on transport errors.
func NewPublicResolver(servers []string) Lookuper {
	c := &chain{}
	for _, s := range servers {
		s := s
		c.rs = append(c.rs, &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, network, s)
			},
		})
	}
	return c
}

type chain struct{ rs []*net.Resolver }

func notFound(err error) bool {
	var de *net.DNSError
	return errors.As(err, &de) && de.IsNotFound
}

func (c *chain) LookupMX(ctx context.Context, name string) (out []*net.MX, err error) {
	for _, r := range c.rs {
		out, err = r.LookupMX(ctx, name)
		if err == nil || notFound(err) {
			return
		}
	}
	return
}

func (c *chain) LookupTXT(ctx context.Context, name string) (out []string, err error) {
	for _, r := range c.rs {
		out, err = r.LookupTXT(ctx, name)
		if err == nil || notFound(err) {
			return
		}
	}
	return
}

// Checker verifies expected records.
type Checker struct {
	R       Lookuper
	Timeout time.Duration
	Now     func() time.Time
}

// Verify checks all records in parallel, each with its own timeout.
// The returned time is when the actual queries completed.
func (c *Checker) Verify(ctx context.Context, exp []Expected) ([]Result, time.Time) {
	res := make([]Result, len(exp))
	var wg sync.WaitGroup
	for i := range exp {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			qctx, cancel := context.WithTimeout(ctx, c.Timeout)
			defer cancel()
			res[i] = c.checkOne(qctx, exp[i])
		}(i)
	}
	wg.Wait()
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	return res, now().UTC()
}

func (c *Checker) checkOne(ctx context.Context, e Expected) Result {
	r := Result{Expected: e, Status: StatusMissing}
	fail := func(err error) Result {
		r.Status = StatusError
		r.Error = err.Error()
		return r
	}
	switch e.Purpose {
	case "mx":
		mxs, err := c.R.LookupMX(ctx, e.Name)
		if err != nil && !notFound(err) {
			return fail(err)
		}
		var hosts []string
		for _, m := range mxs {
			hosts = append(hosts, normHost(m.Host))
		}
		sort.Strings(hosts)
		r.Detected = strings.Join(hosts, ", ")
		if len(hosts) == 0 {
			return r
		}
		want := normHost(strings.Fields(e.Value + " ")[lastField(e.Value)])
		for _, h := range hosts {
			if h == want {
				r.Status = StatusOK
				return r
			}
		}
		r.Status = StatusMismatch
	case "spf":
		txt, err := c.R.LookupTXT(ctx, e.Name)
		if err != nil && !notFound(err) {
			return fail(err)
		}
		var spf []string
		for _, t := range txt {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(t)), "v=spf1") {
				spf = append(spf, t)
			}
		}
		r.Detected = strings.Join(spf, " | ")
		if len(spf) == 0 {
			return r
		}
		need := spfTokens(e.Value)
		for _, s := range spf {
			if containsAll(spfTokens(s), need) {
				r.Status = StatusOK
				return r
			}
		}
		r.Status = StatusMismatch
	case "dkim":
		txt, err := c.R.LookupTXT(ctx, e.Name) // Go's resolver follows CNAME chains
		if err != nil && !notFound(err) {
			return fail(err)
		}
		var found []string
		for _, t := range txt {
			if strings.Contains(strings.ToLower(t), "v=dkim1") || strings.Contains(strings.ToLower(t), "p=") {
				found = append(found, t)
			}
		}
		r.Detected = strings.Join(found, " | ")
		if len(found) == 0 {
			return r
		}
		want := tagValue(e.Value, "p")
		for _, t := range found {
			if want != "" && strings.EqualFold(tagValue(t, "p"), want) {
				r.Status = StatusOK
				return r
			}
		}
		r.Status = StatusMismatch
	case "dmarc":
		txt, err := c.R.LookupTXT(ctx, e.Name)
		if err != nil && !notFound(err) {
			return fail(err)
		}
		var found []string
		for _, t := range txt {
			if strings.HasPrefix(strings.ToLower(strings.TrimSpace(t)), "v=dmarc1") {
				found = append(found, t)
			}
		}
		r.Detected = strings.Join(found, " | ")
		if len(found) == 0 {
			return r
		}
		want := strings.ToLower(tagValue(e.Value, "p"))
		for _, t := range found {
			if strings.ToLower(tagValue(t, "p")) == want {
				r.Status = StatusOK
				return r
			}
		}
		r.Status = StatusMismatch
	default:
		return fail(errors.New("unknown record purpose " + e.Purpose))
	}
	return r
}

// Aggregate derives the domain-level status. "verified" only if every record
// is ok (fail-closed).
func Aggregate(rs []Result) string {
	if len(rs) == 0 {
		return DomainPending
	}
	ok, missing, mismatch, errs := 0, 0, 0, 0
	for _, r := range rs {
		switch r.Status {
		case StatusOK:
			ok++
		case StatusMissing:
			missing++
		case StatusMismatch:
			mismatch++
		default:
			errs++
		}
	}
	switch {
	case ok == len(rs):
		return DomainVerified
	case mismatch > 0:
		return DomainFailed
	case errs > 0:
		return DomainError
	case ok > 0:
		return DomainPropagating
	default:
		return DomainPending
	}
}

// PendingRecords returns the records that are not yet ok.
func PendingRecords(rs []Result) []Result {
	out := []Result{}
	for _, r := range rs {
		if r.Status != StatusOK {
			out = append(out, r)
		}
	}
	return out
}

func lastField(v string) int {
	n := len(strings.Fields(v))
	if n == 0 {
		return 0
	}
	return n - 1
}

func normHost(h string) string { return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), ".")) }

func spfTokens(s string) []string {
	var out []string
	for _, f := range strings.Fields(strings.ToLower(s)) {
		if f == "v=spf1" || f == "-all" || f == "~all" || f == "?all" || f == "+all" || f == "all" {
			continue
		}
		out = append(out, f)
	}
	return out
}

func containsAll(have, need []string) bool {
	set := map[string]bool{}
	for _, h := range have {
		set[h] = true
	}
	for _, n := range need {
		if !set[n] {
			return false
		}
	}
	return true
}

// tagValue extracts a tag from "k=v; k2=v2" records, ignoring whitespace in the
// value (DKIM keys are often split across multiple TXT strings).
func tagValue(rec, tag string) string {
	for _, part := range strings.Split(rec, ";") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) == 2 && strings.EqualFold(strings.TrimSpace(kv[0]), tag) {
			return strings.Join(strings.Fields(kv[1]), "")
		}
	}
	return ""
}
