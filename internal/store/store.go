// Package store is the PostgreSQL repository layer (control plane only).
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

type Store struct{ P *pgxpool.Pool }

func New(p *pgxpool.Pool) *Store { return &Store{P: p} }

func mapErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return ErrConflict
	}
	return err
}

// ---------- domains ----------

type Domain struct {
	ID, Name, Punycode string
	StalwartID         *string
	DKIMSelector       string
	DKIMPrivate        *string // encrypted
	DKIMPublic         *string
	DNSStatus          string
	CatchAll           bool
	CatchAllTarget     *string
	VerifiedAt         *time.Time
	LastChecked        *time.Time
	CreatedAt          time.Time
	DeletedAt          *time.Time
}

const domainCols = `id,name,punycode_name,stalwart_domain_id,dkim_selector,dkim_private_key,dkim_public_key,dns_status,catch_all_enabled,catch_all_target,dns_verified_at,dns_last_checked_at,created_at,deleted_at`

func scanDomain(r pgx.Row) (*Domain, error) {
	var d Domain
	err := r.Scan(&d.ID, &d.Name, &d.Punycode, &d.StalwartID, &d.DKIMSelector, &d.DKIMPrivate, &d.DKIMPublic,
		&d.DNSStatus, &d.CatchAll, &d.CatchAllTarget, &d.VerifiedAt, &d.LastChecked, &d.CreatedAt, &d.DeletedAt)
	if err != nil {
		return nil, mapErr(err)
	}
	return &d, nil
}

func (s *Store) CreateDomain(ctx context.Context, name, puny, selector string, catchAll bool) (*Domain, error) {
	return scanDomain(s.P.QueryRow(ctx, `INSERT INTO domains(name,punycode_name,dkim_selector,catch_all_enabled)
		VALUES($1,$2,$3,$4) RETURNING `+domainCols, name, puny, selector, catchAll))
}

// GetDomain finds an active domain by unicode or punycode name.
func (s *Store) GetDomain(ctx context.Context, name string) (*Domain, error) {
	return scanDomain(s.P.QueryRow(ctx, `SELECT `+domainCols+` FROM domains
		WHERE (name=$1 OR punycode_name=$1) AND deleted_at IS NULL`, name))
}

func (s *Store) GetDomainByID(ctx context.Context, id string) (*Domain, error) {
	return scanDomain(s.P.QueryRow(ctx, `SELECT `+domainCols+` FROM domains WHERE id=$1`, id))
}

func (s *Store) ListDomains(ctx context.Context) ([]*Domain, error) {
	rows, err := s.P.Query(ctx, `SELECT `+domainCols+` FROM domains WHERE deleted_at IS NULL ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Domain
	for rows.Next() {
		d, err := scanDomain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) SetDomainStalwart(ctx context.Context, id, stalwartID, encPriv, pub string) error {
	_, err := s.P.Exec(ctx, `UPDATE domains SET stalwart_domain_id=$2,dkim_private_key=NULLIF($3,''),dkim_public_key=NULLIF($4,''),updated_at=now() WHERE id=$1`,
		id, stalwartID, encPriv, pub)
	return err
}

func (s *Store) SetCatchAll(ctx context.Context, id string, enabled bool, target *string) error {
	_, err := s.P.Exec(ctx, `UPDATE domains SET catch_all_enabled=$2,catch_all_target=$3,updated_at=now() WHERE id=$1`, id, enabled, target)
	return err
}

func (s *Store) SetDNSStatus(ctx context.Context, id, status string, checked time.Time) error {
	_, err := s.P.Exec(ctx, `UPDATE domains SET dns_status=$2::text, dns_last_checked_at=$3::timestamptz,
		dns_verified_at = CASE WHEN $2::text='verified' THEN COALESCE(dns_verified_at,$3::timestamptz) ELSE NULL END, updated_at=now() WHERE id=$1`,
		id, status, checked)
	return err
}

func (s *Store) SoftDeleteDomain(ctx context.Context, id string) error {
	_, err := s.P.Exec(ctx, `UPDATE domains SET deleted_at=now(),updated_at=now() WHERE id=$1 AND deleted_at IS NULL`, id)
	return err
}

func (s *Store) HardDeleteDomain(ctx context.Context, id string) error {
	_, err := s.P.Exec(ctx, `DELETE FROM domains WHERE id=$1 AND deleted_at IS NOT NULL`, id)
	return err
}

func (s *Store) SoftDeletedDomains(ctx context.Context) ([]*Domain, error) {
	rows, err := s.P.Query(ctx, `SELECT `+domainCols+` FROM domains WHERE deleted_at IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Domain
	for rows.Next() {
		d, err := scanDomain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DomainsMissingStalwart returns active domains whose Stalwart registration never completed.
func (s *Store) DomainsMissingStalwart(ctx context.Context, olderThan time.Duration) ([]*Domain, error) {
	rows, err := s.P.Query(ctx, `SELECT `+domainCols+` FROM domains
		WHERE deleted_at IS NULL AND (stalwart_domain_id IS NULL OR dkim_public_key IS NULL)
		AND created_at < now() - $1::interval`, olderThan.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Domain
	for rows.Next() {
		d, err := scanDomain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---------- dns records ----------

type DNSRecord struct {
	ID, DomainID, Type, Name, Purpose, Expected string
	Detected                                    *string
	Status                                      string
	CloudflareID                                *string
	LastChecked                                 *time.Time
}

func (s *Store) UpsertDNSRecord(ctx context.Context, domainID, typ, name, purpose, expected string) error {
	_, err := s.P.Exec(ctx, `INSERT INTO dns_records(domain_id,record_type,record_name,purpose,expected_value)
		VALUES($1,$2,$3,$4,$5)
		ON CONFLICT (domain_id,purpose) DO UPDATE SET record_type=$2,record_name=$3,expected_value=$5,updated_at=now()`,
		domainID, typ, name, purpose, expected)
	return err
}

func (s *Store) ListDNSRecords(ctx context.Context, domainID string) ([]*DNSRecord, error) {
	rows, err := s.P.Query(ctx, `SELECT id,domain_id,record_type,record_name,purpose,expected_value,detected_value,verification_status,cloudflare_record_id,last_checked_at
		FROM dns_records WHERE domain_id=$1 ORDER BY CASE purpose WHEN 'mx' THEN 1 WHEN 'spf' THEN 2 WHEN 'dkim' THEN 3 ELSE 4 END`, domainID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*DNSRecord
	for rows.Next() {
		var r DNSRecord
		if err := rows.Scan(&r.ID, &r.DomainID, &r.Type, &r.Name, &r.Purpose, &r.Expected, &r.Detected, &r.Status, &r.CloudflareID, &r.LastChecked); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

func (s *Store) SetRecordResult(ctx context.Context, domainID, purpose, status, detected string, at time.Time) error {
	_, err := s.P.Exec(ctx, `UPDATE dns_records SET verification_status=$3,detected_value=NULLIF($4,''),last_checked_at=$5,updated_at=now()
		WHERE domain_id=$1 AND purpose=$2`, domainID, purpose, status, detected, at)
	return err
}

func (s *Store) SetRecordCloudflareID(ctx context.Context, domainID, purpose, cfID string) error {
	_, err := s.P.Exec(ctx, `UPDATE dns_records SET cloudflare_record_id=$3,updated_at=now() WHERE domain_id=$1 AND purpose=$2`, domainID, purpose, cfID)
	return err
}

// ---------- mailboxes ----------

type Mailbox struct {
	ID, DomainID, LocalPart, Address string
	StalwartID                       *string
	QuotaBytes, UsedBytes            int64
	UsageMeasuredAt                  *time.Time
	CreatedAt                        time.Time
	DeletedAt                        *time.Time
}

const mbCols = `id,domain_id,local_part,address,stalwart_mailbox_id,quota_bytes,cached_used_bytes,usage_measured_at,created_at,deleted_at`

func scanMailbox(r pgx.Row) (*Mailbox, error) {
	var m Mailbox
	if err := r.Scan(&m.ID, &m.DomainID, &m.LocalPart, &m.Address, &m.StalwartID, &m.QuotaBytes, &m.UsedBytes, &m.UsageMeasuredAt, &m.CreatedAt, &m.DeletedAt); err != nil {
		return nil, mapErr(err)
	}
	return &m, nil
}

func collectMailboxes(rows pgx.Rows, err error) ([]*Mailbox, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Mailbox
	for rows.Next() {
		m, err := scanMailbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) CreateMailbox(ctx context.Context, domainID, local, address string, quota int64) (*Mailbox, error) {
	return scanMailbox(s.P.QueryRow(ctx, `INSERT INTO mailboxes(domain_id,local_part,address,quota_bytes) VALUES($1,$2,$3,$4) RETURNING `+mbCols,
		domainID, local, address, quota))
}

func (s *Store) SetMailboxStalwart(ctx context.Context, id, stalwartID string) error {
	_, err := s.P.Exec(ctx, `UPDATE mailboxes SET stalwart_mailbox_id=$2,usage_measured_at=now(),cached_used_bytes=0,updated_at=now() WHERE id=$1`, id, stalwartID)
	return err
}

func (s *Store) GetMailbox(ctx context.Context, address string) (*Mailbox, error) {
	return scanMailbox(s.P.QueryRow(ctx, `SELECT `+mbCols+` FROM mailboxes WHERE address=$1 AND deleted_at IS NULL AND stalwart_mailbox_id IS NOT NULL`, address))
}

func (s *Store) ListMailboxes(ctx context.Context, domainID string) ([]*Mailbox, error) {
	return collectMailboxes(s.P.Query(ctx, `SELECT `+mbCols+` FROM mailboxes WHERE domain_id=$1 AND deleted_at IS NULL AND stalwart_mailbox_id IS NOT NULL ORDER BY address`, domainID))
}

func (s *Store) ListAllMailboxes(ctx context.Context) ([]*Mailbox, error) {
	return collectMailboxes(s.P.Query(ctx, `SELECT `+mbCols+` FROM mailboxes WHERE deleted_at IS NULL AND stalwart_mailbox_id IS NOT NULL ORDER BY address`))
}

func (s *Store) SetQuota(ctx context.Context, id string, quota int64) error {
	_, err := s.P.Exec(ctx, `UPDATE mailboxes SET quota_bytes=$2,updated_at=now() WHERE id=$1`, id, quota)
	return err
}

func (s *Store) SetUsage(ctx context.Context, id string, used int64, at time.Time) error {
	_, err := s.P.Exec(ctx, `UPDATE mailboxes SET cached_used_bytes=$2,usage_measured_at=$3 WHERE id=$1`, id, used, at)
	return err
}

func (s *Store) SoftDeleteMailbox(ctx context.Context, id string) error {
	_, err := s.P.Exec(ctx, `UPDATE mailboxes SET deleted_at=now(),updated_at=now() WHERE id=$1 AND deleted_at IS NULL`, id)
	return err
}

// HardDeleteMailbox removes a soft-deleted mailbox and its alias target links.
func (s *Store) HardDeleteMailbox(ctx context.Context, id string) error {
	tx, err := s.P.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM alias_targets WHERE mailbox_id=$1`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM mailboxes WHERE id=$1 AND deleted_at IS NOT NULL`, id); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) SoftDeletedMailboxes(ctx context.Context) ([]*Mailbox, error) {
	return collectMailboxes(s.P.Query(ctx, `SELECT `+mbCols+` FROM mailboxes WHERE deleted_at IS NOT NULL`))
}

// StaleUnprovisionedMailboxes are rows whose Stalwart create never confirmed.
func (s *Store) StaleUnprovisionedMailboxes(ctx context.Context, olderThan time.Duration) ([]*Mailbox, error) {
	return collectMailboxes(s.P.Query(ctx, `SELECT `+mbCols+` FROM mailboxes WHERE deleted_at IS NULL AND stalwart_mailbox_id IS NULL AND created_at < now() - $1::interval`, olderThan.String()))
}

func (s *Store) PendingOutbound(ctx context.Context, mailboxID string) (int, error) {
	var n int
	err := s.P.QueryRow(ctx, `SELECT count(*) FROM outbound_queue WHERE mailbox_id=$1 AND status IN ('queued','sending')`, mailboxID).Scan(&n)
	return n, err
}

// ---------- aliases ----------

type Alias struct {
	ID, DomainID, LocalPart, FullAddress string
	Targets                              []string
	TargetIDs                            []string
	CreatedAt                            time.Time
}

func (s *Store) CreateAlias(ctx context.Context, domainID, local, full string, targetIDs []string) (*Alias, error) {
	tx, err := s.P.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var a Alias
	if err := tx.QueryRow(ctx, `INSERT INTO aliases(domain_id,local_part,full_address) VALUES($1,$2,$3) RETURNING id,domain_id,local_part,full_address,created_at`,
		domainID, local, full).Scan(&a.ID, &a.DomainID, &a.LocalPart, &a.FullAddress, &a.CreatedAt); err != nil {
		return nil, mapErr(err)
	}
	for _, t := range targetIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO alias_targets(alias_id,mailbox_id) VALUES($1,$2)`, a.ID, t); err != nil {
			return nil, mapErr(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.GetAliasByID(ctx, a.ID)
}

func (s *Store) loadTargets(ctx context.Context, a *Alias) error {
	rows, err := s.P.Query(ctx, `SELECT m.id,m.address FROM alias_targets t JOIN mailboxes m ON m.id=t.mailbox_id WHERE t.alias_id=$1 ORDER BY m.address`, a.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, addr string
		if err := rows.Scan(&id, &addr); err != nil {
			return err
		}
		a.TargetIDs = append(a.TargetIDs, id)
		a.Targets = append(a.Targets, addr)
	}
	return rows.Err()
}

func (s *Store) GetAliasByID(ctx context.Context, id string) (*Alias, error) {
	var a Alias
	if err := s.P.QueryRow(ctx, `SELECT id,domain_id,local_part,full_address,created_at FROM aliases WHERE id=$1`, id).
		Scan(&a.ID, &a.DomainID, &a.LocalPart, &a.FullAddress, &a.CreatedAt); err != nil {
		return nil, mapErr(err)
	}
	return &a, s.loadTargets(ctx, &a)
}

func (s *Store) GetAlias(ctx context.Context, domainID, local string) (*Alias, error) {
	var a Alias
	if err := s.P.QueryRow(ctx, `SELECT id,domain_id,local_part,full_address,created_at FROM aliases WHERE domain_id=$1 AND local_part=$2 AND deleted_at IS NULL`, domainID, local).
		Scan(&a.ID, &a.DomainID, &a.LocalPart, &a.FullAddress, &a.CreatedAt); err != nil {
		return nil, mapErr(err)
	}
	return &a, s.loadTargets(ctx, &a)
}

func (s *Store) ListAliases(ctx context.Context, domainID string) ([]*Alias, error) {
	rows, err := s.P.Query(ctx, `SELECT id FROM aliases WHERE domain_id=$1 AND deleted_at IS NULL ORDER BY local_part`, domainID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := []*Alias{}
	for _, id := range ids {
		a, err := s.GetAliasByID(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

// AliasesTargeting lists active aliases that forward to the mailbox.
func (s *Store) AliasesTargeting(ctx context.Context, mailboxID string) ([]string, error) {
	rows, err := s.P.Query(ctx, `SELECT a.full_address FROM alias_targets t JOIN aliases a ON a.id=t.alias_id WHERE t.mailbox_id=$1 AND a.deleted_at IS NULL ORDER BY 1`, mailboxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (s *Store) DeleteAlias(ctx context.Context, id string) error {
	_, err := s.P.Exec(ctx, `DELETE FROM aliases WHERE id=$1`, id) // targets cascade
	return err
}

// ---------- jobs ----------

type Job struct {
	ID, Type              string
	DomainID, MailboxID   *string
	Payload               json.RawMessage
	Attempts, MaxAttempts int
}

func (s *Store) Enqueue(ctx context.Context, jobType string, domainID, mailboxID *string, payload any, runAt time.Time) (string, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	if payload == nil {
		b = []byte("{}")
	}
	var id string
	err = s.P.QueryRow(ctx, `INSERT INTO jobs(job_type,domain_id,mailbox_id,payload,run_at) VALUES($1,$2,$3,$4,$5) RETURNING id`,
		jobType, domainID, mailboxID, b, runAt).Scan(&id)
	return id, err
}

// ClaimJob takes one due job with FOR UPDATE SKIP LOCKED.
func (s *Store) ClaimJob(ctx context.Context, worker string) (*Job, error) {
	var j Job
	err := s.P.QueryRow(ctx, `UPDATE jobs SET status='running',locked_at=now(),locked_by=$1,attempts=attempts+1,updated_at=now()
		WHERE id=(SELECT id FROM jobs WHERE status='queued' AND run_at<=now() ORDER BY run_at FOR UPDATE SKIP LOCKED LIMIT 1)
		RETURNING id,job_type,domain_id,mailbox_id,payload,attempts,max_attempts`, worker).
		Scan(&j.ID, &j.Type, &j.DomainID, &j.MailboxID, &j.Payload, &j.Attempts, &j.MaxAttempts)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &j, nil
}

func (s *Store) CompleteJob(ctx context.Context, id string) error {
	_, err := s.P.Exec(ctx, `UPDATE jobs SET status='done',completed_at=now(),locked_at=NULL,locked_by=NULL,last_error=NULL,updated_at=now() WHERE id=$1`, id)
	return err
}

// FailJob re-queues with backoff, or marks failed permanently at max_attempts.
func (s *Store) FailJob(ctx context.Context, j *Job, cause error, nextRun time.Time) error {
	if j.Attempts >= j.MaxAttempts {
		_, err := s.P.Exec(ctx, `UPDATE jobs SET status='failed',completed_at=now(),locked_at=NULL,locked_by=NULL,last_error=$2,updated_at=now() WHERE id=$1`, j.ID, cause.Error())
		return err
	}
	_, err := s.P.Exec(ctx, `UPDATE jobs SET status='queued',run_at=$3,locked_at=NULL,locked_by=NULL,last_error=$2,updated_at=now() WHERE id=$1`, j.ID, cause.Error(), nextRun)
	return err
}

// RecoverStuckJobs re-queues jobs whose lock expired (crashed worker).
func (s *Store) RecoverStuckJobs(ctx context.Context, lockTimeout time.Duration) (int64, error) {
	t, err := s.P.Exec(ctx, `UPDATE jobs SET status='queued',locked_at=NULL,locked_by=NULL,last_error=COALESCE(last_error,'recovered after worker crash'),updated_at=now()
		WHERE status='running' AND locked_at < now() - $1::interval`, lockTimeout.String())
	return t.RowsAffected(), err
}

func (s *Store) HasOpenJob(ctx context.Context, jobType string, domainID *string) (bool, error) {
	var ok bool
	err := s.P.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM jobs WHERE job_type=$1 AND status IN ('queued','running') AND domain_id IS NOT DISTINCT FROM $2)`, jobType, domainID).Scan(&ok)
	return ok, err
}

// ---------- audit + credentials ----------

func (s *Store) Audit(ctx context.Context, actor, action, resType string, resID *string, meta map[string]any, ip string) error {
	if meta == nil {
		meta = map[string]any{}
	}
	b, _ := json.Marshal(meta)
	_, err := s.P.Exec(ctx, `INSERT INTO audit_logs(actor,action,resource_type,resource_id,metadata,ip_address) VALUES($1,$2,$3,$4,$5,$6)`,
		actor, action, resType, resID, b, ip)
	return err
}

func (s *Store) CreateCredential(ctx context.Context, name, hash string) error {
	_, err := s.P.Exec(ctx, `INSERT INTO api_credentials(name,token_hash) VALUES($1,$2)`, name, hash)
	return mapErr(err)
}

// Authenticate returns the credential name for an active token hash and touches last_used_at.
func (s *Store) Authenticate(ctx context.Context, hash string) (string, error) {
	var name string
	err := s.P.QueryRow(ctx, `UPDATE api_credentials SET last_used_at=now() WHERE token_hash=$1 AND active RETURNING name`, hash).Scan(&name)
	return name, mapErr(err)
}

func (s *Store) Ping(ctx context.Context) error { return s.P.Ping(ctx) }
