package cloudflare

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type cfSrv struct {
	recs  []cfRecord
	posts int
}

func (c *cfSrv) handler(w http.ResponseWriter, r *http.Request) {
	ok := func(v any) {
		b, _ := json.Marshal(v)
		w.Write([]byte(`{"success":true,"result":` + string(b) + `}`))
	}
	switch {
	case r.URL.Path == "/zones":
		ok([]map[string]string{{"id": "z1"}})
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/dns_records"):
		var out []cfRecord
		for _, x := range c.recs {
			if x.Type == r.URL.Query().Get("type") && x.Name == r.URL.Query().Get("name") {
				out = append(out, x)
			}
		}
		ok(out)
	case r.Method == "POST":
		c.posts++
		var rec cfRecord
		json.NewDecoder(r.Body).Decode(&rec)
		rec.ID = "id" + string(rune('0'+c.posts))
		c.recs = append(c.recs, rec)
		ok(rec)
	default:
		w.WriteHeader(500)
	}
}

func TestUpsertIdempotentAndNeverOverwrites(t *testing.T) {
	f := &cfSrv{}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()
	c := New("tok", "").WithBase(srv.URL)
	ctx := context.Background()
	spf := Record{Type: "TXT", Name: "example.com", Content: "v=spf1 mx -all", MatchPrefix: "v=spf1"}
	id1, err := c.Upsert(ctx, "example.com", spf, "")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := c.Upsert(ctx, "example.com", spf, "")
	if err != nil || id1 != id2 || f.posts != 1 {
		t.Fatalf("not idempotent: %s %s posts=%d %v", id1, id2, f.posts, err)
	}
	// different SPF already there -> conflict, not overwritten
	other := Record{Type: "TXT", Name: "example.com", Content: "v=spf1 include:google.com -all", MatchPrefix: "v=spf1"}
	if _, err := c.Upsert(ctx, "example.com", other, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	// existing different MX -> conflict
	f.recs = append(f.recs, cfRecord{ID: "old", Type: "MX", Name: "example.com", Content: "old.mail.net", Priority: 5})
	if _, err := c.Upsert(ctx, "example.com", Record{Type: "MX", Name: "example.com", Content: "mail.example.com", Priority: 10}, ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected MX conflict, got %v", err)
	}
}

func TestUnavailable(t *testing.T) {
	c := New("tok", "z").WithBase("http://127.0.0.1:1")
	if _, err := c.Upsert(context.Background(), "example.com", Record{Type: "MX", Name: "example.com", Content: "m"}, ""); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}
