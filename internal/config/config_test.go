package config

import (
	"strings"
	"testing"
)

func base(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://x")
	t.Setenv("STALWART_URL", "http://s")
	t.Setenv("MAIL_HOST", "mail.example.com")
}

func TestDefaults(t *testing.T) {
	base(t)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.JunkThreshold != 5 || c.RejectThreshold != 15 || c.MaxMessageSize != 25<<20 || c.PasswordLength != 16 || c.AliasMaxTargets != 5 {
		t.Fatalf("%+v", c)
	}
}

func TestEnvOverridesAndValidation(t *testing.T) {
	base(t)
	t.Setenv("SPAM_JUNK_THRESHOLD", "7.5")
	c, err := Load()
	if err != nil || c.JunkThreshold != 7.5 {
		t.Fatalf("%v %v", c, err)
	}
	t.Setenv("SPAM_JUNK_THRESHOLD", "-1")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("negative threshold accepted: %v", err)
	}
	t.Setenv("SPAM_JUNK_THRESHOLD", "abc")
	if _, err := Load(); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestMissingRequired(t *testing.T) {
	t.Setenv("DATABASE_URL", "")
	t.Setenv("STALWART_URL", "")
	t.Setenv("MAIL_HOST", "")
	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "DATABASE_URL") {
		t.Fatal(err)
	}
}
