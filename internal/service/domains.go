package service

import (
	"context"
	"time"

	"selfmail/internal/store"
)

type DomainView struct {
	ID             string       `json:"id"`
	Name           string       `json:"name"`
	PunycodeName   string       `json:"punycode_name"`
	CatchAll       bool         `json:"catch_all"`
	CatchAllTarget *string      `json:"catch_all_target"`
	CreatedAt      time.Time    `json:"created_at"`
	DNSRecords     []RecordView `json:"dns_records,omitempty"`
	DNSInfo
	// usage aggregate (GET /domains/{d})
	Usage *DomainUsage `json:"usage,omitempty"`
}

type DomainUsage struct {
	TotalUsedBytes        int64     `json:"total_used_bytes"`
	TotalQuotaBytes       int64     `json:"total_quota_bytes"`
	MailboxCount          int       `json:"mailbox_count"`
	MailboxCountNearQuota int       `json:"mailbox_count_near_quota"`
	MeasuredAt            time.Time `json:"measured_at"`
}

func (s *Service) domainView(ctx context.Context, d *store.Domain) (*DomainView, error) {
	info, err := s.DNSState(ctx, d)
	if err != nil {
		return nil, err
	}
	return &DomainView{ID: d.ID, Name: d.Name, PunycodeName: d.Punycode, CatchAll: d.CatchAll,
		CatchAllTarget: d.CatchAllTarget, CreatedAt: d.CreatedAt, DNSInfo: info}, nil
}

// CreateDomain implements FR-01.
func (s *Service) CreateDomain(ctx context.Context, name string, catchAll bool) (*DomainView, error) {
	uni, ascii, err := NormalizeDomain(name)
	if err != nil {
		return nil, err
	}
	if catchAll {
		return nil, Bad("catch_all cannot be enabled at creation because no mailbox exists yet; create a mailbox, then PATCH /domains/{name} with catch_all_target", nil)
	}
	if _, err := s.Store.GetDomain(ctx, ascii); err == nil {
		return nil, Conflict("domain_exists", "domain "+uni+" already exists", nil)
	}
	d, err := s.Store.CreateDomain(ctx, uni, ascii, s.Cfg.DKIMSelector, false)
	if err != nil {
		return nil, wrap(err, "domain")
	}
	rollback := func() {
		bg := context.WithoutCancel(ctx)
		_ = s.Store.SoftDeleteDomain(bg, d.ID)
		_ = s.Store.HardDeleteDomain(bg, d.ID)
	}
	id, err := s.SW.EnsureDomain(ctx, ascii)
	if err != nil {
		rollback()
		return nil, wrap(err, "stalwart domain")
	}
	selector, pub, err := s.SW.DomainDKIM(ctx, ascii)
	if err != nil {
		// Domain exists in Stalwart but DKIM failed: leave the row; reconcile completes it.
		return nil, wrap(err, "stalwart dkim")
	}
	if err := s.Store.SetDomainStalwart(ctx, d.ID, id, selector, pub); err != nil {
		return nil, err
	}
	d, _ = s.Store.GetDomainByID(ctx, d.ID)
	if err := s.saveExpected(ctx, d); err != nil {
		return nil, err
	}
	if _, err := s.Store.Enqueue(ctx, "provision_dns_records", &d.ID, nil, nil, time.Now()); err != nil {
		return nil, err
	}
	v, err := s.domainView(ctx, d)
	if err != nil {
		return nil, err
	}
	v.DNSRecords, _ = s.storedRecords(ctx, d)
	s.audit(ctx, "create_domain", "domain", &d.ID, map[string]any{"name": ascii})
	return v, nil
}

func (s *Service) GetDomainEntity(ctx context.Context, name string) (*store.Domain, error) {
	_, ascii, err := NormalizeDomain(name)
	if err != nil {
		return nil, err
	}
	d, err := s.Store.GetDomain(ctx, ascii)
	if err != nil {
		return nil, wrap(err, "domain")
	}
	return d, nil
}

// GetDomain implements GET /domains/{name} including the usage aggregate (FR-10).
func (s *Service) GetDomain(ctx context.Context, name string) (*DomainView, error) {
	d, err := s.GetDomainEntity(ctx, name)
	if err != nil {
		return nil, err
	}
	v, err := s.domainView(ctx, d)
	if err != nil {
		return nil, err
	}
	u, err := s.domainUsage(ctx, d)
	if err != nil {
		return nil, err
	}
	v.Usage = u
	return v, nil
}

func (s *Service) ListDomains(ctx context.Context) ([]*DomainView, error) {
	ds, err := s.Store.ListDomains(ctx)
	if err != nil {
		return nil, err
	}
	out := []*DomainView{}
	for _, d := range ds {
		v, err := s.domainView(ctx, d)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *Service) StatusOf(ctx context.Context, name string) (*VerifyResult, error) {
	d, err := s.GetDomainEntity(ctx, name)
	if err != nil {
		return nil, err
	}
	return s.Verify(ctx, d)
}

// PatchCatchAll implements PATCH /domains/{d} (FR-04).
func (s *Service) PatchCatchAll(ctx context.Context, name string, enable bool, target string) (*DomainView, error) {
	d, err := s.GetDomainEntity(ctx, name)
	if err != nil {
		return nil, err
	}
	if enable {
		if target == "" {
			return nil, Bad("catch_all_target is required when catch_all is true", nil)
		}
		local, dom, err := SplitAddress(target)
		if err != nil {
			return nil, err
		}
		addr := local + "@" + dom
		if dom != d.Punycode {
			return nil, Bad("catch_all_target must be a mailbox in the same domain", nil)
		}
		if _, err := s.Store.GetMailbox(ctx, addr); err != nil {
			return nil, Bad("catch_all_target is not an existing mailbox", map[string]any{"invalid_targets": []string{addr}})
		}
		if err := s.SW.SetForwarder(ctx, "@"+d.Punycode, []string{addr}); err != nil {
			return nil, wrap(err, "catch-all")
		}
		if err := s.Store.SetCatchAll(ctx, d.ID, true, &addr); err != nil {
			return nil, err
		}
		d.CatchAll, d.CatchAllTarget = true, &addr
	} else {
		if err := s.SW.DeleteForwarder(ctx, "@"+d.Punycode); err != nil {
			return nil, wrap(err, "catch-all")
		}
		if err := s.Store.SetCatchAll(ctx, d.ID, false, nil); err != nil {
			return nil, err
		}
		d.CatchAll, d.CatchAllTarget = false, nil
	}
	s.audit(ctx, "patch_catch_all", "domain", &d.ID, map[string]any{"catch_all": enable, "target": target})
	return s.domainView(ctx, d)
}

// DeleteDomainResult: Done=true means 204; otherwise cleanup was deferred (202).
type DeleteDomainResult struct {
	Done  bool
	JobID string
}

// DeleteDomain implements FR-01 deletion. Without force, active mailboxes block with 409.
func (s *Service) DeleteDomain(ctx context.Context, name string, force bool) (*DeleteDomainResult, error) {
	d, err := s.GetDomainEntity(ctx, name)
	if err != nil {
		return nil, err
	}
	mbs, err := s.Store.ListMailboxes(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	if len(mbs) > 0 && !force {
		var blocking []string
		for _, m := range mbs {
			blocking = append(blocking, m.Address)
		}
		return nil, Conflict("domain_has_mailboxes", "domain still has mailboxes; delete them first or pass ?force=true", map[string]any{"blocking_mailboxes": blocking})
	}
	// Mailbox removal must succeed in Stalwart before the domain row can go.
	for _, m := range mbs {
		if err := s.Store.SoftDeleteMailbox(ctx, m.ID); err != nil {
			return nil, err
		}
	}
	if err := s.Store.SoftDeleteDomain(ctx, d.ID); err != nil {
		return nil, err
	}
	s.audit(ctx, "delete_domain", "domain", &d.ID, map[string]any{"name": d.Punycode, "force": force, "mailboxes": len(mbs)})
	if err := s.CleanupDomain(ctx, d); err != nil {
		id, qerr := s.Store.Enqueue(ctx, "delete_domain", &d.ID, nil, nil, time.Now().Add(s.Cfg.JobBaseBackoff))
		if qerr != nil {
			return nil, qerr
		}
		s.Log.Warn("domain delete deferred", "domain", d.Punycode, "err", err)
		return &DeleteDomainResult{Done: false, JobID: id}, nil
	}
	return &DeleteDomainResult{Done: true}, nil
}

// CleanupDomain removes everything for a soft-deleted domain, in Stalwart and DNS, then hard-deletes.
// It is idempotent and shared by the API and the worker.
func (s *Service) CleanupDomain(ctx context.Context, d *store.Domain) error {
	mbs, err := s.Store.SoftDeletedMailboxes(ctx)
	if err != nil {
		return err
	}
	for _, m := range mbs {
		if m.DomainID != d.ID {
			continue
		}
		if err := s.CleanupMailbox(ctx, m); err != nil {
			return err
		}
	}
	aliases, err := s.Store.ListAliases(ctx, d.ID)
	if err != nil {
		return err
	}
	for _, a := range aliases {
		if err := s.SW.DeleteForwarder(ctx, a.FullAddress); err != nil {
			return err
		}
		if err := s.Store.DeleteAlias(ctx, a.ID); err != nil {
			return err
		}
	}
	if err := s.SW.DeleteForwarder(ctx, "@"+d.Punycode); err != nil {
		return err
	}
	if s.CF != nil {
		recs, err := s.Store.ListDNSRecords(ctx, d.ID)
		if err != nil {
			return err
		}
		for _, r := range recs {
			if r.CloudflareID != nil {
				if err := s.CF.Delete(ctx, d.Punycode, *r.CloudflareID); err != nil {
					return err
				}
			}
		}
	}
	if err := s.SW.DeleteDomain(ctx, d.Punycode); err != nil {
		return err
	}
	return s.Store.HardDeleteDomain(ctx, d.ID)
}
