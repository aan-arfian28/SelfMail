package service

import (
	"context"
	"strings"
	"time"

	"selfmail/internal/store"
)

func lower(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

type AliasView struct {
	Address   string    `json:"address"`
	LocalPart string    `json:"local_part"`
	Targets   []string  `json:"targets"`
	CreatedAt time.Time `json:"created_at"`
	DNSInfo
}

func (s *Service) aliasView(ctx context.Context, a *store.Alias, d *store.Domain) (*AliasView, error) {
	info, err := s.DNSState(ctx, d)
	if err != nil {
		return nil, err
	}
	return &AliasView{Address: a.FullAddress, LocalPart: a.LocalPart, Targets: a.Targets, CreatedAt: a.CreatedAt, DNSInfo: info}, nil
}

// CreateAlias implements FR-04 (1..N targets, mailbox targets only, no loops).
func (s *Service) CreateAlias(ctx context.Context, domain, local string, targets []string) (*AliasView, error) {
	d, err := s.GetDomainEntity(ctx, domain)
	if err != nil {
		return nil, err
	}
	local = lower(local)
	if err := validLocalPart(local); err != nil {
		return nil, err
	}
	if len(targets) == 0 {
		return nil, Bad("targets must contain at least 1 mailbox", nil)
	}
	if len(targets) > s.Cfg.AliasMaxTargets {
		return nil, Bad("too many targets", map[string]any{"max_targets": s.Cfg.AliasMaxTargets, "given": len(targets)})
	}
	seen := map[string]bool{}
	var ids, addrs, invalid []string
	for _, t := range targets {
		tl, td, err := SplitAddress(t)
		if err != nil {
			invalid = append(invalid, t)
			continue
		}
		addr := tl + "@" + td
		if seen[addr] {
			continue
		}
		seen[addr] = true
		m, err := s.Store.GetMailbox(ctx, addr) // aliases are not mailboxes -> alias-to-alias loops are rejected here
		if err != nil {
			invalid = append(invalid, t)
			continue
		}
		ids = append(ids, m.ID)
		addrs = append(addrs, addr)
	}
	if len(invalid) > 0 {
		return nil, Bad("some targets are not existing mailboxes (alias targets must be mailboxes, not aliases)", map[string]any{"invalid_targets": invalid})
	}
	if _, err := s.Store.GetMailbox(ctx, local+"@"+d.Punycode); err == nil {
		return nil, Conflict("mailbox_exists", "a mailbox with this local_part already exists; mailbox takes priority over alias", nil)
	}
	full := local + "@" + d.Punycode
	a, err := s.Store.CreateAlias(ctx, d.ID, local, full, ids)
	if err != nil {
		return nil, wrap(err, "alias")
	}
	if err := s.SW.SetForwarder(ctx, full, addrs); err != nil {
		_ = s.Store.DeleteAlias(context.WithoutCancel(ctx), a.ID)
		return nil, wrap(err, "alias")
	}
	s.audit(ctx, "create_alias", "alias", &a.ID, map[string]any{"address": full, "targets": addrs})
	return s.aliasView(ctx, a, d)
}

func (s *Service) ListAliases(ctx context.Context, domain string) ([]*AliasView, error) {
	d, err := s.GetDomainEntity(ctx, domain)
	if err != nil {
		return nil, err
	}
	as, err := s.Store.ListAliases(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	out := []*AliasView{}
	for _, a := range as {
		v, err := s.aliasView(ctx, a, d)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (s *Service) DeleteAlias(ctx context.Context, domain, local string) error {
	d, err := s.GetDomainEntity(ctx, domain)
	if err != nil {
		return err
	}
	a, err := s.Store.GetAlias(ctx, d.ID, lower(local))
	if err != nil {
		return wrap(err, "alias")
	}
	if err := s.SW.DeleteForwarder(ctx, a.FullAddress); err != nil {
		return wrap(err, "alias")
	}
	if err := s.Store.DeleteAlias(ctx, a.ID); err != nil {
		return err
	}
	s.audit(ctx, "delete_alias", "alias", &a.ID, map[string]any{"address": a.FullAddress})
	return nil
}
