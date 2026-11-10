package service

import (
	"context"
	"sort"
	"sync"
	"time"

	"selfmail/internal/store"
)

// refreshUsage refreshes stale cache entries with bounded concurrency; any
// Stalwart failure aborts with 503 (fail-closed).
func (s *Service) refreshUsage(ctx context.Context, ms []*store.Mailbox) error {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	var mu sync.Mutex
	var first error
	for _, m := range ms {
		if m.UsageMeasuredAt != nil && time.Since(*m.UsageMeasuredAt) <= s.Cfg.UsageCacheTTL {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(m *store.Mailbox) {
			defer wg.Done()
			defer func() { <-sem }()
			if _, _, err := s.usage(ctx, m); err != nil {
				mu.Lock()
				if first == nil {
					first = err
				}
				mu.Unlock()
			}
		}(m)
	}
	wg.Wait()
	return first
}

func (s *Service) domainUsage(ctx context.Context, d *store.Domain) (*DomainUsage, error) {
	ms, err := s.Store.ListMailboxes(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	if err := s.refreshUsage(ctx, ms); err != nil {
		return nil, err
	}
	u := &DomainUsage{MeasuredAt: time.Now().UTC(), MailboxCount: len(ms)}
	for _, m := range ms {
		u.TotalUsedBytes += m.UsedBytes
		u.TotalQuotaBytes += m.QuotaBytes
		if m.QuotaBytes > 0 && float64(m.UsedBytes)/float64(m.QuotaBytes) > 0.8 {
			u.MailboxCountNearQuota++
		}
	}
	return u, nil
}

type UsageDomain struct {
	Name string `json:"name"`
	DomainUsage
	DNSInfo
}

type UsageSnapshot struct {
	MeasuredAt   time.Time      `json:"measured_at"`
	Domains      []UsageDomain  `json:"domains"`
	TopMailboxes []MailboxBrief `json:"top_mailboxes"`
}

type MailboxBrief struct {
	Address     string  `json:"address"`
	UsedBytes   int64   `json:"used_bytes"`
	QuotaBytes  int64   `json:"quota_bytes"`
	PercentUsed float64 `json:"percent_used"`
	OverQuota   bool    `json:"over_quota"`
	DNSStatus   string  `json:"dns_status"`
}

// Usage implements GET /usage: installation snapshot, no N+1 thanks to the cache.
func (s *Service) Usage(ctx context.Context) (*UsageSnapshot, error) {
	ds, err := s.Store.ListDomains(ctx)
	if err != nil {
		return nil, err
	}
	snap := &UsageSnapshot{MeasuredAt: time.Now().UTC(), Domains: []UsageDomain{}, TopMailboxes: []MailboxBrief{}}
	var all []MailboxBrief
	for _, d := range ds {
		u, err := s.domainUsage(ctx, d)
		if err != nil {
			return nil, err
		}
		info, err := s.DNSState(ctx, d)
		if err != nil {
			return nil, err
		}
		snap.Domains = append(snap.Domains, UsageDomain{Name: d.Name, DomainUsage: *u, DNSInfo: info})
		ms, _ := s.Store.ListMailboxes(ctx, d.ID)
		for _, m := range ms {
			all = append(all, MailboxBrief{Address: m.Address, UsedBytes: m.UsedBytes, QuotaBytes: m.QuotaBytes,
				PercentUsed: percent(m.UsedBytes, m.QuotaBytes), OverQuota: m.UsedBytes > m.QuotaBytes, DNSStatus: info.DNSStatus})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].UsedBytes > all[j].UsedBytes })
	if len(all) > 10 {
		all = all[:10]
	}
	if all != nil {
		snap.TopMailboxes = all
	}
	return snap, nil
}
