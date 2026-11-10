CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE domains (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name varchar(255) NOT NULL,
  punycode_name varchar(255) NOT NULL,
  stalwart_domain_id varchar(255),
  dkim_selector varchar(64) NOT NULL DEFAULT 'default',
  dkim_private_key text,
  dkim_public_key text,
  dns_status varchar(32) NOT NULL DEFAULT 'pending'
    CHECK (dns_status IN ('pending','propagating','verified','failed','error')),
  catch_all_enabled boolean NOT NULL DEFAULT false,
  catch_all_target varchar(320),
  dns_verified_at timestamptz,
  dns_last_checked_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  deleted_at timestamptz
);
CREATE UNIQUE INDEX domains_name_active ON domains (name) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX domains_puny_active ON domains (punycode_name) WHERE deleted_at IS NULL;
CREATE INDEX domains_dns_status ON domains (dns_status);

CREATE TABLE mailboxes (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  domain_id uuid NOT NULL REFERENCES domains(id) ON DELETE CASCADE,
  local_part varchar(64) NOT NULL,
  address varchar(320) NOT NULL,
  stalwart_mailbox_id varchar(255),
  quota_bytes bigint NOT NULL CHECK (quota_bytes > 0),
  cached_used_bytes bigint NOT NULL DEFAULT 0,
  usage_measured_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  deleted_at timestamptz
);
CREATE UNIQUE INDEX mailboxes_addr_active ON mailboxes (address) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX mailboxes_local_active ON mailboxes (domain_id, local_part) WHERE deleted_at IS NULL;

CREATE TABLE aliases (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  domain_id uuid NOT NULL REFERENCES domains(id) ON DELETE CASCADE,
  local_part varchar(64) NOT NULL,
  full_address varchar(320) NOT NULL,
  is_catch_all boolean NOT NULL DEFAULT false,
  active boolean NOT NULL DEFAULT true,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  deleted_at timestamptz
);
CREATE UNIQUE INDEX aliases_local_active ON aliases (domain_id, local_part) WHERE deleted_at IS NULL;
CREATE INDEX aliases_catch_all ON aliases (domain_id, is_catch_all);

CREATE TABLE alias_targets (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  alias_id uuid NOT NULL REFERENCES aliases(id) ON DELETE CASCADE,
  mailbox_id uuid NOT NULL REFERENCES mailboxes(id) ON DELETE RESTRICT,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (alias_id, mailbox_id)
);

CREATE TABLE dns_records (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  domain_id uuid NOT NULL REFERENCES domains(id) ON DELETE CASCADE,
  record_type varchar(16) NOT NULL CHECK (record_type IN ('MX','TXT','A','CNAME')),
  record_name varchar(255) NOT NULL,
  purpose varchar(16) NOT NULL CHECK (purpose IN ('mx','spf','dkim','dmarc')),
  expected_value text NOT NULL,
  detected_value text,
  verification_status varchar(16) NOT NULL DEFAULT 'missing'
    CHECK (verification_status IN ('ok','mismatch','missing','error')),
  cloudflare_record_id varchar(128),
  last_checked_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (domain_id, purpose)
);

CREATE TABLE jobs (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  job_type varchar(64) NOT NULL CHECK (job_type IN
    ('provision_dns_records','verify_dns','provision_mailbox','delete_mailbox','delete_domain','sync_usage','spam_feedback','reconcile')),
  status varchar(16) NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','running','done','failed')),
  domain_id uuid REFERENCES domains(id) ON DELETE CASCADE,
  mailbox_id uuid REFERENCES mailboxes(id) ON DELETE CASCADE,
  payload jsonb NOT NULL DEFAULT '{}'::jsonb,
  attempts integer NOT NULL DEFAULT 0,
  max_attempts integer NOT NULL DEFAULT 5,
  run_at timestamptz NOT NULL DEFAULT now(),
  locked_at timestamptz,
  locked_by varchar(255),
  completed_at timestamptz,
  last_error text,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX jobs_poll ON jobs (status, run_at);
CREATE INDEX jobs_domain ON jobs (domain_id);
CREATE INDEX jobs_mailbox ON jobs (mailbox_id);

CREATE TABLE spam_feedbacks (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  mailbox_id uuid NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
  message_id varchar(998) NOT NULL,
  feedback_type varchar(16) NOT NULL CHECK (feedback_type IN ('ham','spam')),
  subject_hash text,
  from_address varchar(320),
  status varchar(16) NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','sent','skipped','failed')),
  attempts integer NOT NULL DEFAULT 0,
  last_error text,
  processed_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (mailbox_id, message_id)
);
CREATE INDEX spam_feedbacks_poll ON spam_feedbacks (status, created_at);

CREATE TABLE outbound_queue (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  mailbox_id uuid NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
  message_id varchar(998) NOT NULL,
  recipient varchar(320) NOT NULL,
  message_size bigint NOT NULL,
  retry_count integer NOT NULL DEFAULT 0,
  status varchar(16) NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','sending','sent','bounced','failed')),
  first_attempt_at timestamptz,
  next_attempt_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  last_error text,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX outbound_poll ON outbound_queue (status, next_attempt_at);
CREATE INDEX outbound_mailbox ON outbound_queue (mailbox_id);
CREATE INDEX outbound_expires ON outbound_queue (expires_at);

CREATE TABLE audit_logs (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  actor varchar(255) NOT NULL,
  action varchar(64) NOT NULL,
  resource_type varchar(64) NOT NULL,
  resource_id uuid,
  metadata jsonb NOT NULL DEFAULT '{}'::jsonb,
  ip_address varchar(64),
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_resource ON audit_logs (resource_type, resource_id);
CREATE INDEX audit_created ON audit_logs (created_at);

CREATE TABLE api_credentials (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name varchar(128) NOT NULL UNIQUE,
  token_hash varchar(255) NOT NULL UNIQUE,
  active boolean NOT NULL DEFAULT true,
  last_used_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);
