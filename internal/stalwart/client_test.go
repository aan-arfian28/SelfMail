package stalwart

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestUnavailableOnConnectionFailure(t *testing.T) {
	c := New("http://127.0.0.1:1", "", "", time.Second)
	if err := c.Ping(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
}

func TestGetRetriesPostDoesNot(t *testing.T) {
	var gets, posts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			atomic.AddInt32(&gets, 1)
		} else {
			atomic.AddInt32(&posts, 1)
		}
		w.WriteHeader(502)
	}))
	defer srv.Close()
	c := New(srv.URL, "u", "p", time.Second)
	if err := c.Ping(context.Background()); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if gets != 3 {
		t.Fatalf("gets=%d", gets)
	}
	if _, err := c.CreateMailbox(context.Background(), "a@b.com", "pw", 1); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
	if posts != 1 {
		t.Fatalf("non-idempotent POST was retried: %d", posts)
	}
}

func TestEnsureDomainIsIdempotent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"error":"fieldAlreadyExists","item":"name"}`))
	}))
	defer srv.Close()
	id, err := New(srv.URL, "", "", time.Second).EnsureDomain(context.Background(), "example.com")
	if err != nil || id != "example.com" {
		t.Fatalf("%q %v", id, err)
	}
}

func TestUsageAndDKIMParsing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/principal/a@b.com":
			w.Write([]byte(`{"data":{"id":5,"type":"individual","name":"a@b.com","usedQuota":1234,"quota":5000}}`))
		case "/api/dkim":
			w.Write([]byte(`{"data":null}`))
		case "/api/dns/records/b.com":
			w.Write([]byte(`{"data":[{"type":"MX","name":"b.com.","content":"10 mail.b.com."},{"type":"TXT","name":"default._domainkey.b.com.","content":"v=DKIM1; k=rsa; p=AAA BBB"}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "", "", time.Second)
	used, err := c.Usage(context.Background(), "a@b.com")
	if err != nil || used != 1234 {
		t.Fatalf("%d %v", used, err)
	}
	pub, err := c.EnsureDKIM(context.Background(), "b.com", "default")
	if err != nil || pub != "AAABBB" {
		t.Fatalf("%q %v", pub, err)
	}
	if _, err := c.Usage(context.Background(), "nobody@b.com"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}
