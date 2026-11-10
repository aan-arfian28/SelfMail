// Package autoconfig serves the Thunderbird autoconfig XML (FR-11).
package autoconfig

import (
	"encoding/xml"
	"net/http"
	"strings"
)

type clientConfig struct {
	XMLName  xml.Name `xml:"clientConfig"`
	Version  string   `xml:"version,attr"`
	Provider provider `xml:"emailProvider"`
}

type provider struct {
	ID               string   `xml:"id,attr"`
	Domain           string   `xml:"domain"`
	DisplayName      string   `xml:"displayName"`
	DisplayShortName string   `xml:"displayShortName"`
	Incoming         server   `xml:"incomingServer"`
	Outgoing         []server `xml:"outgoingServer"`
}

type server struct {
	Type           string `xml:"type,attr"`
	Hostname       string `xml:"hostname"`
	Port           int    `xml:"port"`
	SocketType     string `xml:"socketType"`
	Username       string `xml:"username"`
	Authentication string `xml:"authentication"`
}

// Build renders the XML for a domain. host is the public mail hostname.
func Build(domain, host string) ([]byte, error) {
	cfg := clientConfig{Version: "1.1", Provider: provider{
		ID: domain, Domain: domain, DisplayName: domain + " Mail", DisplayShortName: domain,
		Incoming: server{Type: "imap", Hostname: host, Port: 993, SocketType: "SSL", Username: "%EMAILADDRESS%", Authentication: "password-cleartext"},
		Outgoing: []server{
			{Type: "smtp", Hostname: host, Port: 587, SocketType: "STARTTLS", Username: "%EMAILADDRESS%", Authentication: "password-cleartext"},
			{Type: "smtp", Hostname: host, Port: 465, SocketType: "SSL", Username: "%EMAILADDRESS%", Authentication: "password-cleartext"},
		},
	}}
	b, err := xml.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	return append([]byte(xml.Header), b...), nil
}

// DomainFor derives the domain from ?emailaddress= or the autoconfig.<domain> Host.
func DomainFor(r *http.Request) string {
	if e := r.URL.Query().Get("emailaddress"); strings.Contains(e, "@") {
		return strings.ToLower(e[strings.LastIndex(e, "@")+1:])
	}
	h := strings.ToLower(r.Host)
	if i := strings.LastIndex(h, ":"); i > 0 {
		h = h[:i]
	}
	return strings.TrimPrefix(h, "autoconfig.")
}
