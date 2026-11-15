package stalwart

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

const sampleZone = "v1-ed25519-20261110._domainkey.test.local. IN TXT \"v=DKIM1; k=ed25519; h=sha256; p=EDKEY=\"\n" +
	"v1-rsa-20261110._domainkey.test.local. IN TXT (\n    \"v=DKIM1; k=rsa; h=sha256; p=AAAA\"\n    \"BBBB\"\n)\n" +
	"test.local. IN MX 10 mail.test.local.\n"

func TestParseDKIMPicksRSAAndJoinsSegments(t *testing.T) {
	sel, pub, err := ParseDKIM(sampleZone)
	if err != nil || sel != "v1-rsa-20261110" || pub != "AAAABBBB" {
		t.Fatalf("%q %q %v", sel, pub, err)
	}
	if _, _, err := ParseDKIM("test.local. IN MX 10 mail.test.local."); err == nil {
		t.Fatal("expected error without DKIM record")
	}
}

func TestUnavailableOnConnectionFailure(t *testing.T) {
	c := New("http://127.0.0.1:1", "", "", time.Second)
	if err := c.Ping(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
	if _, err := c.EnsureDomain(context.Background(), "x.com"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
}

// Integration test against a real, bootstrapped Stalwart (>= 1.x).
// STALWART_TEST_URL=http://localhost:18081 STALWART_TEST_USER=... STALWART_TEST_PASS=...
func TestAgainstRealStalwart(t *testing.T) {
	url := os.Getenv("STALWART_TEST_URL")
	if url == "" {
		t.Skip("STALWART_TEST_URL not set")
	}
	c := New(url, os.Getenv("STALWART_TEST_USER"), os.Getenv("STALWART_TEST_PASS"), 10*time.Second)
	ctx := context.Background()
	dom := fmt.Sprintf("it%d.test", time.Now().UnixNano()%1e9)

	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	id1, err := c.EnsureDomain(ctx, dom)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := c.EnsureDomain(ctx, dom) // idempotent: no duplicate
	if err != nil || id1 != id2 {
		t.Fatalf("not idempotent: %s %s %v", id1, id2, err)
	}
	sel, pub, err := c.DomainDKIM(ctx, dom)
	if err != nil || sel == "" || len(pub) < 100 {
		t.Fatalf("dkim: %q %d %v", sel, len(pub), err)
	}

	a, b := "halo@"+dom, "other@"+dom
	if _, err := c.CreateMailbox(ctx, a, "Str0ng-Password!x", 1000000); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateMailbox(ctx, a, "Str0ng-Password!x", 1000000); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate mailbox: %v", err)
	}
	if _, err := c.CreateMailbox(ctx, b, "Str0ng-Password!x", 1000000); err != nil {
		t.Fatal(err)
	}
	used, err := c.Usage(ctx, a)
	if err != nil || used != 0 {
		t.Fatalf("usage %d %v", used, err)
	}
	if _, err := c.Usage(ctx, "ghost@"+dom); !errors.Is(err, ErrNotFound) {
		t.Fatalf("filter must not leak other accounts: %v", err)
	}
	if err := c.SetQuota(ctx, a, 2000000); err != nil {
		t.Fatal(err)
	}
	if err := c.SetPassword(ctx, a, "An0ther-Password!y"); err != nil {
		t.Fatal(err)
	}

	if err := c.SetForwarder(ctx, "info@"+dom, []string{a, b}); err != nil {
		t.Fatal(err)
	}
	if err := c.SetForwarder(ctx, "info@"+dom, []string{a}); err != nil { // replace
		t.Fatal(err)
	}
	if err := c.SetForwarder(ctx, "@"+dom, []string{a}); err != nil { // catch-all
		t.Fatal(err)
	}
	if err := c.DeleteForwarder(ctx, "@"+dom); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteForwarder(ctx, "info@"+dom); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteForwarder(ctx, "info@"+dom); err != nil { // idempotent
		t.Fatal(err)
	}

	n, err := c.DeleteMailbox(ctx, a)
	if err != nil || n != 0 {
		t.Fatalf("delete: %d %v", n, err)
	}
	if _, err := c.DeleteMailbox(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteDomain(ctx, dom); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteDomain(ctx, dom); err != nil { // idempotent
		t.Fatal(err)
	}
}
