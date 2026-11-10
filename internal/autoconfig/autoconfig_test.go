package autoconfig

import (
	"encoding/xml"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuild(t *testing.T) {
	b, err := Build("example.com", "mail.example.com")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"<port>993</port>", "<port>587</port>", "<port>465</port>", "%EMAILADDRESS%", `version="1.1"`, "mail.example.com"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q", want)
		}
	}
	var v clientConfig
	if err := xml.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
}

func TestDomainFor(t *testing.T) {
	r := httptest.NewRequest("GET", "/mail/config-v1.1.xml?emailaddress=halo@Domain.com", nil)
	if DomainFor(r) != "domain.com" {
		t.Fatal(DomainFor(r))
	}
	r = httptest.NewRequest("GET", "/mail/config-v1.1.xml", nil)
	r.Host = "autoconfig.domain.com:80"
	if DomainFor(r) != "domain.com" {
		t.Fatal(DomainFor(r))
	}
}
