// Package service holds the business logic (orchestration between the DB,
// Stalwart, Cloudflare and DNS verification).
package service

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"selfmail/internal/cloudflare"
	"selfmail/internal/config"
	"selfmail/internal/dnscheck"
	"selfmail/internal/stalwart"
	"selfmail/internal/store"
)

type Service struct {
	Cfg   *config.Config
	Store *store.Store
	SW    stalwart.API
	CF    cloudflare.API // nil when Cloudflare is not configured
	DNS   *dnscheck.Checker
	Log   *slog.Logger
}

type actorKey struct{}

// Actor identifies who performs a mutation (for audit logs).
type Actor struct{ Name, IP string }

func WithActor(ctx context.Context, a Actor) context.Context {
	return context.WithValue(ctx, actorKey{}, a)
}

func (s *Service) audit(ctx context.Context, action, resType string, resID *string, meta map[string]any) {
	a, _ := ctx.Value(actorKey{}).(Actor)
	if a.Name == "" {
		a.Name = "system"
	}
	// Callers must never put passwords in meta.
	if err := s.Store.Audit(ctx, a.Name, action, resType, resID, meta, a.IP); err != nil {
		s.Log.Error("audit write failed", "action", action, "err", err)
	}
}

// ---------- DNS views ----------

type RecordView struct {
	Purpose  string `json:"purpose"`
	Type     string `json:"type"`
	Name     string `json:"name"`
	Expected string `json:"expected"`
	Detected string `json:"detected"`
	Status   string `json:"status"`
}

// DNSInfo is embedded in every domain/mailbox/alias/usage response (FR-18).
type DNSInfo struct {
	DNSStatus      string       `json:"dns_status"`
	PendingRecords []RecordView `json:"pending_records"`
	CheckedAt      *time.Time   `json:"dns_checked_at"`
}

func (s *Service) expectedRecords(d *store.Domain) []dnscheck.Expected {
	var spf []string
	if s.Cfg.ServerIP != "" {
		spf = append(spf, "ip4:"+s.Cfg.ServerIP)
	}
	if s.Cfg.SPFInclude != "" {
		spf = append(spf, "include:"+s.Cfg.SPFInclude)
	}
	if len(spf) == 0 {
		spf = append(spf, "mx")
	}
	pub := ""
	if d.DKIMPublic != nil {
		pub = *d.DKIMPublic
	}
	return []dnscheck.Expected{
		{Purpose: "mx", Type: "MX", Name: d.Punycode, Value: "10 " + s.Cfg.MailHost},
		{Purpose: "spf", Type: "TXT", Name: d.Punycode, Value: "v=spf1 " + strings.Join(spf, " ") + " -all"},
		{Purpose: "dkim", Type: "TXT", Name: d.DKIMSelector + "._domainkey." + d.Punycode, Value: "v=DKIM1; k=rsa; p=" + pub},
		{Purpose: "dmarc", Type: "TXT", Name: "_dmarc." + d.Punycode, Value: "v=DMARC1; p=" + s.Cfg.DMARCPolicy},
	}
}

func (s *Service) saveExpected(ctx context.Context, d *store.Domain) error {
	for _, e := range s.expectedRecords(d) {
		if e.Purpose == "dkim" && (d.DKIMPublic == nil || *d.DKIMPublic == "") {
			continue // key not ready; never publish a record without the real key
		}
		if err := s.Store.UpsertDNSRecord(ctx, d.ID, e.Type, e.Name, e.Purpose, e.Value); err != nil {
			return err
		}
	}
	return nil
}

func toView(r *store.DNSRecord) RecordView {
	v := RecordView{Purpose: r.Purpose, Type: r.Type, Name: r.Name, Expected: r.Expected, Status: r.Status}
	if r.Detected != nil {
		v.Detected = *r.Detected
	}
	return v
}

func (s *Service) storedRecords(ctx context.Context, d *store.Domain) ([]RecordView, error) {
	rs, err := s.Store.ListDNSRecords(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	out := make([]RecordView, 0, len(rs))
	for _, r := range rs {
		out = append(out, toView(r))
	}
	return out, nil
}

// VerifyResult is the response of GET /status and POST /verify.
type VerifyResult struct {
	DNSStatus string       `json:"dns_status"`
	Records   []RecordView `json:"records"`
	Pending   []RecordView `json:"pending_records"`
	CheckedAt time.Time    `json:"checked_at"`
}

// Verify performs an actual DNS query (never served from cache) and persists the outcome.
func (s *Service) Verify(ctx context.Context, d *store.Domain) (*VerifyResult, error) {
	if err := s.saveExpected(ctx, d); err != nil {
		return nil, err
	}
	stored, err := s.Store.ListDNSRecords(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	exp := make([]dnscheck.Expected, 0, len(stored))
	for _, r := range stored {
		exp = append(exp, dnscheck.Expected{Purpose: r.Purpose, Type: r.Type, Name: r.Name, Value: r.Expected})
	}
	results, at := s.DNS.Verify(ctx, exp)
	status := dnscheck.Aggregate(results)
	if d.DKIMPublic == nil || *d.DKIMPublic == "" {
		// A domain without a DKIM key can never be verified.
		if status == dnscheck.DomainVerified {
			status = dnscheck.DomainPropagating
		}
		results = append(results, dnscheck.Result{Expected: dnscheck.Expected{Purpose: "dkim", Type: "TXT",
			Name: d.DKIMSelector + "._domainkey." + d.Punycode}, Status: dnscheck.StatusMissing})
	}
	for _, r := range results {
		if r.Expected.Value == "" {
			continue
		}
		if err := s.Store.SetRecordResult(ctx, d.ID, r.Purpose, r.Status, r.Detected, at); err != nil {
			return nil, err
		}
	}
	if err := s.Store.SetDNSStatus(ctx, d.ID, status, at); err != nil {
		return nil, err
	}
	d.DNSStatus, d.LastChecked = status, &at
	out := &VerifyResult{DNSStatus: status, CheckedAt: at, Records: []RecordView{}, Pending: []RecordView{}}
	for _, r := range results {
		v := RecordView{Purpose: r.Purpose, Type: r.Type, Name: r.Name, Expected: r.Value, Detected: r.Detected, Status: r.Status}
		out.Records = append(out.Records, v)
		if r.Status != dnscheck.StatusOK {
			out.Pending = append(out.Pending, v)
		}
	}
	return out, nil
}

// DNSState returns the shared cached DNS status, refreshing from a real query
// when older than DNS_STATUS_TTL (FR-18).
func (s *Service) DNSState(ctx context.Context, d *store.Domain) (DNSInfo, error) {
	if d.LastChecked == nil || time.Since(*d.LastChecked) > s.Cfg.DNSStatusTTL {
		if _, err := s.Verify(ctx, d); err != nil {
			return DNSInfo{}, err
		}
	}
	recs, err := s.storedRecords(ctx, d)
	if err != nil {
		return DNSInfo{}, err
	}
	info := DNSInfo{DNSStatus: d.DNSStatus, PendingRecords: []RecordView{}, CheckedAt: d.LastChecked}
	if d.DNSStatus != dnscheck.DomainVerified {
		for _, r := range recs {
			if r.Status != dnscheck.StatusOK {
				info.PendingRecords = append(info.PendingRecords, r)
			}
		}
		if len(info.PendingRecords) == 0 { // never claim completion while not verified
			info.PendingRecords = recs
		}
	}
	return info, nil
}

// DNSRecords lists records to create manually (FR-02 /dns-records).
func (s *Service) DNSRecords(ctx context.Context, d *store.Domain) ([]RecordView, error) {
	if err := s.saveExpected(ctx, d); err != nil {
		return nil, err
	}
	return s.storedRecords(ctx, d)
}
