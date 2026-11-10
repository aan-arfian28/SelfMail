package service

import (
	"context"
	"errors"
	"math"
	"time"

	"selfmail/internal/secret"
	"selfmail/internal/stalwart"
	"selfmail/internal/store"
)

type MailboxView struct {
	Address         string    `json:"address"`
	QuotaBytes      int64     `json:"quota_bytes"`
	UsedBytes       int64     `json:"used_bytes"`
	PercentUsed     float64   `json:"percent_used"`
	OverQuota       bool      `json:"over_quota"`
	MeasuredAt      time.Time `json:"measured_at"`
	CreatedAt       time.Time `json:"created_at"`
	InitialPassword string    `json:"initial_password,omitempty"`
	DNSInfo
}

func percent(used, quota int64) float64 {
	if quota <= 0 {
		return 0
	}
	return math.Round(float64(used)/float64(quota)*10000) / 100
}

// usage returns used bytes, served from cache when fresher than USAGE_CACHE_TTL,
// else read live from Stalwart. Stalwart failure -> 503, never a stale/zero value (FR-10).
func (s *Service) usage(ctx context.Context, m *store.Mailbox) (int64, time.Time, error) {
	if m.UsageMeasuredAt != nil && time.Since(*m.UsageMeasuredAt) <= s.Cfg.UsageCacheTTL {
		return m.UsedBytes, *m.UsageMeasuredAt, nil
	}
	used, err := s.SW.Usage(ctx, m.Address)
	if err != nil {
		return 0, time.Time{}, wrap(err, "mailbox usage")
	}
	now := time.Now().UTC()
	if err := s.Store.SetUsage(ctx, m.ID, used, now); err != nil {
		return 0, time.Time{}, err
	}
	m.UsedBytes, m.UsageMeasuredAt = used, &now
	return used, now, nil
}

func (s *Service) mailboxView(ctx context.Context, m *store.Mailbox, d *store.Domain) (*MailboxView, error) {
	used, at, err := s.usage(ctx, m)
	if err != nil {
		return nil, err
	}
	info, err := s.DNSState(ctx, d)
	if err != nil {
		return nil, err
	}
	return &MailboxView{Address: m.Address, QuotaBytes: m.QuotaBytes, UsedBytes: used, PercentUsed: percent(used, m.QuotaBytes),
		OverQuota: used > m.QuotaBytes, MeasuredAt: at, CreatedAt: m.CreatedAt, DNSInfo: info}, nil
}

func (s *Service) mailboxByAddress(ctx context.Context, addr string) (*store.Mailbox, *store.Domain, error) {
	local, dom, err := SplitAddress(addr)
	if err != nil {
		return nil, nil, err
	}
	m, err := s.Store.GetMailbox(ctx, local+"@"+dom)
	if err != nil {
		return nil, nil, wrap(err, "mailbox")
	}
	d, err := s.Store.GetDomainByID(ctx, m.DomainID)
	if err != nil {
		return nil, nil, wrap(err, "domain")
	}
	return m, d, nil
}

// CreateMailbox implements FR-03.
func (s *Service) CreateMailbox(ctx context.Context, domain, local string, quota int64, password string) (*MailboxView, error) {
	d, err := s.GetDomainEntity(ctx, domain)
	if err != nil {
		return nil, err
	}
	local = lower(local)
	if err := validLocalPart(local); err != nil {
		return nil, err
	}
	if quota <= 0 {
		return nil, Bad("quota_bytes must be greater than 0", nil)
	}
	generated := false
	if password == "" {
		if password, err = secret.GeneratePassword(s.Cfg.PasswordLength); err != nil {
			return nil, err
		}
		generated = true
	} else if err := secret.ValidatePassword(password); err != nil {
		return nil, Bad(err.Error(), nil)
	}
	if _, err := s.Store.GetAlias(ctx, d.ID, local); err == nil {
		return nil, Conflict("alias_exists", "an alias with this local_part already exists in the domain", nil)
	}
	address := local + "@" + d.Punycode
	m, err := s.Store.CreateMailbox(ctx, d.ID, local, address, quota)
	if err != nil {
		return nil, wrap(err, "mailbox")
	}
	id, err := s.SW.CreateMailbox(ctx, address, password, quota)
	if err != nil {
		if errors.Is(err, stalwart.ErrConflict) {
			s.dropMailboxRow(ctx, m)
			return nil, Conflict("mailbox_exists", "mailbox already exists in Stalwart; reconciliation required", nil)
		}
		s.dropMailboxRow(ctx, m)
		return nil, wrap(err, "mailbox")
	}
	if err := s.Store.SetMailboxStalwart(ctx, m.ID, id); err != nil {
		return nil, err
	}
	m, _ = s.Store.GetMailbox(ctx, address)
	s.audit(ctx, "create_mailbox", "mailbox", &m.ID, map[string]any{"address": address, "quota_bytes": quota})
	v, err := s.mailboxView(ctx, m, d)
	if err != nil {
		return nil, err
	}
	if generated {
		v.InitialPassword = password // returned once; never logged or stored
	}
	return v, nil
}

func (s *Service) dropMailboxRow(ctx context.Context, m *store.Mailbox) {
	bg := context.WithoutCancel(ctx)
	_ = s.Store.SoftDeleteMailbox(bg, m.ID)
	_ = s.Store.HardDeleteMailbox(bg, m.ID)
}

func (s *Service) GetMailbox(ctx context.Context, addr string) (*MailboxView, error) {
	m, d, err := s.mailboxByAddress(ctx, addr)
	if err != nil {
		return nil, err
	}
	return s.mailboxView(ctx, m, d)
}

func (s *Service) ListMailboxes(ctx context.Context, domain string) ([]*MailboxView, error) {
	d, err := s.GetDomainEntity(ctx, domain)
	if err != nil {
		return nil, err
	}
	ms, err := s.Store.ListMailboxes(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	if err := s.refreshUsage(ctx, ms); err != nil {
		return nil, err
	}
	out := []*MailboxView{}
	for _, m := range ms {
		v, err := s.mailboxView(ctx, m, d)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *Service) PatchQuota(ctx context.Context, addr string, quota int64) (*MailboxView, error) {
	if quota <= 0 {
		return nil, Bad("quota_bytes must be greater than 0", nil)
	}
	m, d, err := s.mailboxByAddress(ctx, addr)
	if err != nil {
		return nil, err
	}
	// Always compare against live usage, not cache.
	used, err := s.SW.Usage(ctx, m.Address)
	if err != nil {
		return nil, wrap(err, "mailbox usage")
	}
	if quota < used {
		return nil, Conflict("quota_below_usage", "quota below current usage", map[string]any{"used_bytes": used, "requested_quota_bytes": quota})
	}
	if err := s.SW.SetQuota(ctx, m.Address, quota); err != nil {
		return nil, wrap(err, "mailbox quota")
	}
	if err := s.Store.SetQuota(ctx, m.ID, quota); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	_ = s.Store.SetUsage(ctx, m.ID, used, now)
	m.QuotaBytes, m.UsedBytes, m.UsageMeasuredAt = quota, used, &now
	s.audit(ctx, "patch_mailbox", "mailbox", &m.ID, map[string]any{"quota_bytes": quota})
	return s.mailboxView(ctx, m, d)
}

func (s *Service) ResetPassword(ctx context.Context, addr string) (string, error) {
	m, _, err := s.mailboxByAddress(ctx, addr)
	if err != nil {
		return "", err
	}
	pw, err := secret.GeneratePassword(s.Cfg.PasswordLength)
	if err != nil {
		return "", err
	}
	if err := s.SW.SetPassword(ctx, m.Address, pw); err != nil {
		return "", wrap(err, "password reset")
	}
	s.audit(ctx, "reset_password", "mailbox", &m.ID, map[string]any{"address": m.Address}) // no password
	return pw, nil
}

type DeleteMailboxResult struct {
	Done            bool
	DeletedMessages int64
	JobID           string
}

func (s *Service) DeleteMailbox(ctx context.Context, addr string) (*DeleteMailboxResult, error) {
	m, d, err := s.mailboxByAddress(ctx, addr)
	if err != nil {
		return nil, err
	}
	var blockers []string
	if aliases, err := s.Store.AliasesTargeting(ctx, m.ID); err == nil {
		blockers = append(blockers, aliases...)
	}
	if d.CatchAllTarget != nil && *d.CatchAllTarget == m.Address {
		blockers = append(blockers, "@"+d.Punycode+" (catch-all)")
	}
	if len(blockers) > 0 {
		return nil, Conflict("mailbox_in_use", "mailbox is the target of aliases/catch-all; remove them first", map[string]any{"blocking": blockers})
	}
	if err := s.Store.SoftDeleteMailbox(ctx, m.ID); err != nil {
		return nil, err
	}
	s.audit(ctx, "delete_mailbox", "mailbox", &m.ID, map[string]any{"address": m.Address})
	count, err := s.cleanupMailbox(ctx, m)
	if err != nil {
		id, qerr := s.Store.Enqueue(ctx, "delete_mailbox", &d.ID, &m.ID, nil, time.Now().Add(s.Cfg.JobBaseBackoff))
		if qerr != nil {
			return nil, qerr
		}
		s.Log.Warn("mailbox delete deferred", "address", m.Address, "err", err)
		return &DeleteMailboxResult{Done: false, JobID: id}, nil
	}
	return &DeleteMailboxResult{Done: true, DeletedMessages: count}, nil
}

func (s *Service) cleanupMailbox(ctx context.Context, m *store.Mailbox) (int64, error) {
	count, err := s.SW.DeleteMailbox(ctx, m.Address)
	if err != nil {
		return 0, err
	}
	return count, s.Store.HardDeleteMailbox(ctx, m.ID)
}

// CleanupMailbox is the idempotent worker/reconcile entry point.
func (s *Service) CleanupMailbox(ctx context.Context, m *store.Mailbox) error {
	_, err := s.cleanupMailbox(ctx, m)
	return err
}
