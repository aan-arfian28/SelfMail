package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"selfmail/internal/cloudflare"
	"selfmail/internal/dnscheck"
	"selfmail/internal/store"
)

// RunJob executes one job; returning an error triggers retry with backoff (FR-17).
func (s *Service) RunJob(ctx context.Context, j *store.Job) error {
	switch j.Type {
	case "provision_dns_records":
		return s.jobProvisionDNS(ctx, j)
	case "verify_dns":
		return s.jobVerifyDNS(ctx, j)
	case "delete_mailbox":
		return s.jobDeleteMailbox(ctx, j)
	case "delete_domain":
		return s.jobDeleteDomain(ctx, j)
	case "reconcile":
		return s.Reconcile(ctx)
	default:
		return fmt.Errorf("no handler for job type %q", j.Type)
	}
}

func (s *Service) jobProvisionDNS(ctx context.Context, j *store.Job) error {
	if j.DomainID == nil {
		return errors.New("job without domain")
	}
	d, err := s.Store.GetDomainByID(ctx, *j.DomainID)
	if err != nil {
		return nil // domain removed meanwhile
	}
	if d.DeletedAt != nil {
		return nil
	}
	if s.CF == nil {
		s.Log.Info("cloudflare not configured; create records manually via GET /domains/{name}/dns-records", "domain", d.Punycode)
		return nil
	}
	if err := s.saveExpected(ctx, d); err != nil {
		return err
	}
	recs, err := s.Store.ListDNSRecords(ctx, d.ID)
	if err != nil {
		return err
	}
	var retryable error
	conflicts := 0
	for _, r := range recs {
		rec := cloudflare.Record{Type: r.Type, Name: r.Name, Content: r.Expected}
		switch r.Purpose {
		case "mx":
			rec.Priority, rec.Content = 10, s.Cfg.MailHost
		case "spf":
			rec.MatchPrefix = "v=spf1"
		case "dkim":
			rec.MatchPrefix = "v=dkim1"
		case "dmarc":
			rec.MatchPrefix = "v=dmarc1"
		}
		known := ""
		if r.CloudflareID != nil {
			known = *r.CloudflareID
		}
		id, err := s.CF.Upsert(ctx, d.Punycode, rec, known) // idempotent (FR-14)
		switch {
		case err == nil:
			if err := s.Store.SetRecordCloudflareID(ctx, d.ID, r.Purpose, id); err != nil {
				return err
			}
		case errors.Is(err, cloudflare.ErrConflict):
			conflicts++
			s.Log.Warn("existing DNS record differs; not overwriting", "domain", d.Punycode, "purpose", r.Purpose, "err", err)
		default:
			retryable = err
		}
	}
	if retryable != nil {
		return retryable // status stays pending; retried with backoff
	}
	if conflicts == 0 && d.DNSStatus == dnscheck.DomainPending {
		_ = s.Store.SetDNSStatus(ctx, d.ID, dnscheck.DomainPropagating, time.Now().UTC())
	}
	deadline := time.Now().Add(24 * time.Hour)
	_, err = s.Store.Enqueue(ctx, "verify_dns", &d.ID, nil, map[string]any{"deadline": deadline}, time.Now().Add(30*time.Second))
	return err
}

// jobVerifyDNS polls every 30s up to 24h until verified.
func (s *Service) jobVerifyDNS(ctx context.Context, j *store.Job) error {
	if j.DomainID == nil {
		return errors.New("job without domain")
	}
	d, err := s.Store.GetDomainByID(ctx, *j.DomainID)
	if err != nil || d.DeletedAt != nil {
		return nil
	}
	res, err := s.Verify(ctx, d)
	if err != nil {
		return err
	}
	if res.DNSStatus == dnscheck.DomainVerified {
		return nil
	}
	var p struct {
		Deadline time.Time `json:"deadline"`
	}
	_ = json.Unmarshal(j.Payload, &p)
	if !p.Deadline.IsZero() && time.Now().After(p.Deadline) {
		s.Log.Warn("dns verification polling gave up", "domain", d.Punycode, "status", res.DNSStatus)
		return nil
	}
	_, err = s.Store.Enqueue(ctx, "verify_dns", &d.ID, nil, map[string]any{"deadline": p.Deadline}, time.Now().Add(30*time.Second))
	return err
}

func (s *Service) jobDeleteMailbox(ctx context.Context, j *store.Job) error {
	if j.MailboxID == nil {
		return errors.New("job without mailbox")
	}
	mbs, err := s.Store.SoftDeletedMailboxes(ctx)
	if err != nil {
		return err
	}
	for _, m := range mbs {
		if m.ID == *j.MailboxID {
			return s.CleanupMailbox(ctx, m)
		}
	}
	return nil // already gone
}

func (s *Service) jobDeleteDomain(ctx context.Context, j *store.Job) error {
	if j.DomainID == nil {
		return errors.New("job without domain")
	}
	d, err := s.Store.GetDomainByID(ctx, *j.DomainID)
	if err != nil {
		return nil
	}
	return s.CleanupDomain(ctx, d)
}

// Reconcile heals states left by crashes between a Stalwart call and a DB commit (FR-14/15).
func (s *Service) Reconcile(ctx context.Context) error {
	var firstErr error
	note := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	// 1. Soft-deleted mailboxes still present in Stalwart -> delete there, then hard delete.
	mbs, err := s.Store.SoftDeletedMailboxes(ctx)
	note(err)
	for _, m := range mbs {
		note(s.CleanupMailbox(ctx, m))
	}
	// 2. Soft-deleted domains.
	ds, err := s.Store.SoftDeletedDomains(ctx)
	note(err)
	for _, d := range ds {
		note(s.CleanupDomain(ctx, d))
	}
	// 3. Domains whose Stalwart registration/DKIM never completed: finish (idempotent, no duplicates).
	pend, err := s.Store.DomainsMissingStalwart(ctx, 2*time.Minute)
	note(err)
	for _, d := range pend {
		id, err := s.SW.EnsureDomain(ctx, d.Punycode)
		if err != nil {
			note(err)
			continue
		}
		selector, pub, err := s.SW.DomainDKIM(ctx, d.Punycode)
		if err != nil {
			note(err)
			continue
		}
		if err := s.Store.SetDomainStalwart(ctx, d.ID, id, selector, pub); err != nil {
			note(err)
			continue
		}
		if d2, err := s.Store.GetDomainByID(ctx, d.ID); err == nil {
			note(s.saveExpected(ctx, d2))
		}
		_, err = s.Store.Enqueue(ctx, "provision_dns_records", &d.ID, nil, nil, time.Now())
		note(err)
	}
	// 4. Mailboxes whose creation never confirmed: the password was never handed out, so abandon them.
	stale, err := s.Store.StaleUnprovisionedMailboxes(ctx, 2*time.Minute)
	note(err)
	for _, m := range stale {
		if _, err := s.SW.DeleteMailbox(ctx, m.Address); err != nil {
			note(err)
			continue
		}
		s.dropMailboxRow(ctx, m)
	}
	return firstErr
}
