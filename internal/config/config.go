// Package config loads runtime configuration from environment variables (FR-22).
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr  string
	DatabaseURL string

	StalwartURL      string
	StalwartUser     string
	StalwartPassword string
	StalwartTimeout  time.Duration

	CloudflareToken  string
	CloudflareZoneID string // optional: when empty, zone is looked up per domain

	MailHost     string // public hostname of mail server, e.g. mail.example.com
	SPFInclude   string // optional extra include for SPF
	ServerIP     string // used in SPF as ip4 when set
	DKIMSelector string
	DMARCPolicy  string

	JunkThreshold               float64
	RejectThreshold             float64
	MaxMessageSize              int64
	SubmissionRateLimitPerHour  int
	InboundRateLimitPerMinPerIP int
	FeedbackRateLimitPerMin     int
	OutboundMaxAge              time.Duration
	OutboundBackoff             []time.Duration

	DNSTimeout   time.Duration
	DNSResolvers []string
	DNSStatusTTL time.Duration

	UsageCacheTTL time.Duration

	AliasMaxTargets int
	PasswordLength  int

	WorkerPollInterval  time.Duration
	JobLockTimeout      time.Duration
	JobBaseBackoff      time.Duration
	RunWorker           bool
	APIRateLimitPerHour int
}

func Load() (*Config, error) {
	var errs []error
	str := func(k, d string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return d
	}
	f := func(k string, d float64) float64 {
		v := os.Getenv(k)
		if v == "" {
			return d
		}
		x, err := strconv.ParseFloat(v, 64)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", k, err))
			return d
		}
		return x
	}
	i := func(k string, d int64) int64 {
		v := os.Getenv(k)
		if v == "" {
			return d
		}
		x, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", k, err))
			return d
		}
		return x
	}
	dur := func(k string, d time.Duration) time.Duration {
		v := os.Getenv(k)
		if v == "" {
			return d
		}
		x, err := time.ParseDuration(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", k, err))
			return d
		}
		return x
	}

	c := &Config{
		ListenAddr:       str("LISTEN_ADDR", ":8080"),
		DatabaseURL:      os.Getenv("DATABASE_URL"),
		StalwartURL:      strings.TrimRight(os.Getenv("STALWART_URL"), "/"),
		StalwartUser:     os.Getenv("STALWART_USER"),
		StalwartPassword: os.Getenv("STALWART_PASSWORD"),
		StalwartTimeout:  dur("STALWART_TIMEOUT", 10*time.Second),
		CloudflareToken:  os.Getenv("CLOUDFLARE_API_TOKEN"),
		CloudflareZoneID: os.Getenv("CLOUDFLARE_ZONE_ID"),
		MailHost:         os.Getenv("MAIL_HOST"),
		SPFInclude:       os.Getenv("SPF_INCLUDE"),
		ServerIP:         os.Getenv("SERVER_IP"),
		DKIMSelector:     str("DKIM_SELECTOR", "default"),
		DMARCPolicy:      str("DMARC_POLICY", "quarantine"),

		JunkThreshold:               f("SPAM_JUNK_THRESHOLD", 5.0),
		RejectThreshold:             f("SPAM_REJECT_THRESHOLD", 15.0),
		MaxMessageSize:              i("MAX_MESSAGE_SIZE", 25*1024*1024),
		SubmissionRateLimitPerHour:  int(i("SUBMISSION_RATE_LIMIT_PER_HOUR", 200)),
		InboundRateLimitPerMinPerIP: int(i("INBOUND_RATE_LIMIT_PER_MIN_PER_IP", 100)),
		FeedbackRateLimitPerMin:     int(i("FEEDBACK_RATE_LIMIT_PER_MIN", 100)),
		OutboundMaxAge:              dur("OUTBOUND_MAX_AGE", 5*24*time.Hour),

		DNSTimeout:   dur("DNS_TIMEOUT", 5*time.Second),
		DNSStatusTTL: dur("DNS_STATUS_TTL", 5*time.Minute),

		UsageCacheTTL: dur("USAGE_CACHE_TTL", 60*time.Second),

		AliasMaxTargets: int(i("ALIAS_MAX_TARGETS", 5)),
		PasswordLength:  int(i("PASSWORD_LENGTH", 16)),

		WorkerPollInterval:  dur("WORKER_POLL_INTERVAL", 5*time.Second),
		JobLockTimeout:      dur("JOB_LOCK_TIMEOUT", 5*time.Minute),
		JobBaseBackoff:      dur("JOB_BASE_BACKOFF", 10*time.Second),
		RunWorker:           str("RUN_WORKER", "true") != "false",
		APIRateLimitPerHour: int(i("API_RATE_LIMIT_PER_HOUR", 1000)),
	}
	c.DNSResolvers = splitList(str("DNS_RESOLVERS", "1.1.1.1:53,8.8.8.8:53"))
	c.OutboundBackoff = []time.Duration{5 * time.Minute, 15 * time.Minute, time.Hour, 4 * time.Hour}
	if v := os.Getenv("OUTBOUND_BACKOFF"); v != "" {
		c.OutboundBackoff = nil
		for _, p := range splitList(v) {
			d, err := time.ParseDuration(p)
			if err != nil {
				errs = append(errs, fmt.Errorf("OUTBOUND_BACKOFF: %w", err))
				continue
			}
			c.OutboundBackoff = append(c.OutboundBackoff, d)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Validate fails fast with explicit errors (FR-22).
func (c *Config) Validate() error {
	var errs []error
	req := func(name, v string) {
		if v == "" {
			errs = append(errs, fmt.Errorf("%s is required", name))
		}
	}
	req("DATABASE_URL", c.DatabaseURL)
	req("STALWART_URL", c.StalwartURL)
	req("MAIL_HOST", c.MailHost)
	if c.JunkThreshold < 0 || c.RejectThreshold < 0 {
		errs = append(errs, errors.New("spam thresholds must not be negative"))
	}
	if c.RejectThreshold < c.JunkThreshold {
		errs = append(errs, errors.New("SPAM_REJECT_THRESHOLD must be >= SPAM_JUNK_THRESHOLD"))
	}
	if c.MaxMessageSize <= 0 {
		errs = append(errs, errors.New("MAX_MESSAGE_SIZE must be > 0"))
	}
	if c.SubmissionRateLimitPerHour <= 0 || c.InboundRateLimitPerMinPerIP <= 0 || c.FeedbackRateLimitPerMin <= 0 {
		errs = append(errs, errors.New("rate limits must be > 0"))
	}
	if c.AliasMaxTargets < 1 {
		errs = append(errs, errors.New("ALIAS_MAX_TARGETS must be >= 1"))
	}
	if c.PasswordLength < 12 {
		errs = append(errs, errors.New("PASSWORD_LENGTH must be >= 12"))
	}
	if c.DNSTimeout <= 0 || c.UsageCacheTTL <= 0 || c.DNSStatusTTL <= 0 {
		errs = append(errs, errors.New("invalid DNS/usage timing configuration"))
	}
	if len(c.OutboundBackoff) == 0 || c.OutboundMaxAge <= 0 {
		errs = append(errs, errors.New("invalid outbound queue configuration"))
	}
	return errors.Join(errs...)
}
