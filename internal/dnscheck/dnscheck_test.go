package dnscheck

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

type fake struct {
	mx  map[string][]*net.MX
	txt map[string][]string
	err error
}

func (f fake) LookupMX(_ context.Context, n string) ([]*net.MX, error) {
	if f.err != nil {
		return nil, f.err
	}
	if v, ok := f.mx[n]; ok {
		return v, nil
	}
	return nil, &net.DNSError{IsNotFound: true}
}
func (f fake) LookupTXT(_ context.Context, n string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	if v, ok := f.txt[n]; ok {
		return v, nil
	}
	return nil, &net.DNSError{IsNotFound: true}
}

var exp = []Expected{
	{"mx", "MX", "example.com", "10 mail.example.com"},
	{"spf", "TXT", "example.com", "v=spf1 ip4:1.2.3.4 -all"},
	{"dkim", "TXT", "default._domainkey.example.com", "v=DKIM1; k=rsa; p=ABCD"},
	{"dmarc", "TXT", "_dmarc.example.com", "v=DMARC1; p=quarantine"},
}

func run(f fake) ([]Result, string) {
	c := &Checker{R: f, Timeout: time.Second}
	r, _ := c.Verify(context.Background(), exp)
	return r, Aggregate(r)
}

func TestAllOK(t *testing.T) {
	r, agg := run(fake{
		mx: map[string][]*net.MX{"example.com": {{Host: "mail.example.com.", Pref: 10}}},
		txt: map[string][]string{
			"example.com":                    {"v=spf1 include:other.com ip4:1.2.3.4 ~all"},
			"default._domainkey.example.com": {"v=DKIM1; k=rsa; p=ABCD"},
			"_dmarc.example.com":             {"v=DMARC1; p=quarantine; rua=mailto:x@y"},
		},
	})
	if agg != DomainVerified {
		t.Fatalf("got %s: %+v", agg, r)
	}
	if len(PendingRecords(r)) != 0 {
		t.Fatal("pending should be empty")
	}
}

func TestMissingIsPending(t *testing.T) {
	_, agg := run(fake{})
	if agg != DomainPending {
		t.Fatal(agg)
	}
}

func TestOldMXIsMismatchNotOK(t *testing.T) {
	r, agg := run(fake{mx: map[string][]*net.MX{"example.com": {{Host: "old.other.net.", Pref: 5}}}})
	if r[0].Status != StatusMismatch || agg != DomainFailed {
		t.Fatalf("%+v %s", r[0], agg)
	}
}

func TestResolverDownFailsClosed(t *testing.T) {
	r, agg := run(fake{err: errors.New("i/o timeout")})
	for _, x := range r {
		if x.Status != StatusError {
			t.Fatalf("expected error got %s", x.Status)
		}
	}
	if agg == DomainVerified {
		t.Fatal("must not verify")
	}
}

func TestPartialIsPropagating(t *testing.T) {
	_, agg := run(fake{mx: map[string][]*net.MX{"example.com": {{Host: "mail.example.com."}}}})
	if agg != DomainPropagating {
		t.Fatal(agg)
	}
}

func TestSPFWithoutOurTokenIsMismatch(t *testing.T) {
	r, _ := run(fake{txt: map[string][]string{"example.com": {"v=spf1 include:google.com -all"}}})
	if r[1].Status != StatusMismatch {
		t.Fatal(r[1].Status)
	}
}
