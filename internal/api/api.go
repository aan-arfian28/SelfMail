// Package api is the HTTP layer: routing, auth, error mapping.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"selfmail/internal/autoconfig"
	"selfmail/internal/config"
	"selfmail/internal/secret"
	"selfmail/internal/service"
	"selfmail/internal/store"
)

type Server struct {
	Svc *service.Service
	St  *store.Store
	Cfg *config.Config
	Log *slog.Logger

	rl rateLimiter
}

func New(svc *service.Service, st *store.Store, cfg *config.Config, log *slog.Logger) *Server {
	return &Server{Svc: svc, St: st, Cfg: cfg, Log: log, rl: rateLimiter{hits: map[string]*window{}}}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// public
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /readyz", s.readyz)
	mux.HandleFunc("GET /mail/config-v1.1.xml", s.autoconfig)
	mux.HandleFunc("GET /.well-known/autoconfig/mail/config-v1.1.xml", s.autoconfig)

	// admin (bearer token)
	a := func(h http.HandlerFunc) http.Handler { return s.auth(h) }
	mux.Handle("POST /domains", a(s.createDomain))
	mux.Handle("GET /domains", a(s.listDomains))
	mux.Handle("GET /domains/{name}", a(s.getDomain))
	mux.Handle("PATCH /domains/{name}", a(s.patchDomain))
	mux.Handle("DELETE /domains/{name}", a(s.deleteDomain))
	mux.Handle("GET /domains/{name}/status", a(s.domainStatus))
	mux.Handle("POST /domains/{name}/verify", a(s.domainStatus))
	mux.Handle("GET /domains/{name}/dns-records", a(s.dnsRecords))

	mux.Handle("POST /domains/{domain}/mailboxes", a(s.createMailbox))
	mux.Handle("GET /domains/{domain}/mailboxes", a(s.listMailboxes))
	mux.Handle("GET /mailboxes/{address}", a(s.getMailbox))
	mux.Handle("PATCH /mailboxes/{address}", a(s.patchMailbox))
	mux.Handle("POST /mailboxes/{address}/password/reset", a(s.resetPassword))
	mux.Handle("DELETE /mailboxes/{address}", a(s.deleteMailbox))

	mux.Handle("POST /domains/{domain}/aliases", a(s.createAlias))
	mux.Handle("GET /domains/{domain}/aliases", a(s.listAliases))
	mux.Handle("DELETE /domains/{domain}/aliases/{local_part}", a(s.deleteAlias))

	mux.Handle("GET /usage", a(s.usage))
	return s.recoverer(s.logging(mux))
}

// ---------- middleware ----------

type ctxKey struct{}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		tok, ok := strings.CutPrefix(h, "Bearer ")
		if !ok || strings.TrimSpace(tok) == "" {
			writeErr(w, &service.Error{Status: 401, Code: "unauthorized", Message: "missing bearer token"})
			return
		}
		name, err := s.St.Authenticate(r.Context(), secret.HashToken(strings.TrimSpace(tok)))
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, &service.Error{Status: 401, Code: "unauthorized", Message: "invalid or inactive token"})
			return
		}
		if err != nil {
			s.fail(w, err)
			return
		}
		if retry := s.rl.allow(name, s.Cfg.APIRateLimitPerHour); retry > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			writeErr(w, &service.Error{Status: 429, Code: "rate_limited", Message: "API rate limit exceeded", RetryAfter: retry})
			return
		}
		ctx := service.WithActor(r.Context(), service.Actor{Name: name, IP: clientIP(r)})
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, ctxKey{}, name)))
	})
}

func clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

type window struct {
	start time.Time
	n     int
}

type rateLimiter struct {
	mu   sync.Mutex
	hits map[string]*window
}

// allow returns 0 if permitted, else seconds until the window resets.
func (l *rateLimiter) allow(key string, limit int) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	w := l.hits[key]
	if w == nil || now.Sub(w.start) >= time.Hour {
		w = &window{start: now}
		l.hits[key] = w
	}
	if w.n >= limit {
		return int(time.Hour.Seconds()-now.Sub(w.start).Seconds()) + 1
	}
	w.n++
	return 0
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(c int) { r.code = c; r.ResponseWriter.WriteHeader(c) }

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: 200}
		next.ServeHTTP(rec, r)
		// Path only: never log bodies (they may contain passwords).
		s.Log.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.code, "ms", time.Since(start).Milliseconds())
	})
}

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.Log.Error("panic", "err", v, "stack", string(debug.Stack()))
				writeErr(w, &service.Error{Status: 500, Code: "internal_error", Message: "internal error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, e *service.Error) {
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(e.RetryAfter))
	}
	writeJSON(w, e.Status, map[string]any{"error": e})
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	var se *service.Error
	if errors.As(err, &se) {
		writeErr(w, se)
		return
	}
	s.Log.Error("unhandled error", "err", err)
	writeErr(w, &service.Error{Status: 500, Code: "internal_error", Message: "internal error"})
}

func decode(r *http.Request, v any) *service.Error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return service.Bad("invalid JSON body: "+err.Error(), nil)
	}
	return nil
}

// ---------- public ----------

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{"status": "ok"})
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	comps := map[string]string{"database": "ok", "stalwart": "ok"}
	ready := true
	if err := s.St.Ping(ctx); err != nil {
		comps["database"], ready = "unavailable", false
	}
	if err := s.Svc.SW.Ping(ctx); err != nil {
		comps["stalwart"], ready = "unavailable", false
	}
	if !ready {
		w.Header().Set("Retry-After", "30")
		writeJSON(w, 503, map[string]any{"status": "unavailable", "components": comps})
		return
	}
	writeJSON(w, 200, map[string]any{"status": "ok", "components": comps})
}

func (s *Server) autoconfig(w http.ResponseWriter, r *http.Request) {
	dom := autoconfig.DomainFor(r)
	if _, err := s.Svc.GetDomainEntity(r.Context(), dom); err != nil {
		s.fail(w, err)
		return
	}
	b, err := autoconfig.Build(dom, s.Cfg.MailHost)
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	_, _ = w.Write(b)
}

// ---------- domains ----------

func (s *Server) createDomain(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name            string `json:"name"`
		CatchAllEnabled bool   `json:"catch_all_enabled"`
	}
	if e := decode(r, &in); e != nil {
		writeErr(w, e)
		return
	}
	v, err := s.Svc.CreateDomain(r.Context(), in.Name, in.CatchAllEnabled)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 201, v)
}

func (s *Server) listDomains(w http.ResponseWriter, r *http.Request) {
	v, err := s.Svc.ListDomains(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"domains": v})
}

func (s *Server) getDomain(w http.ResponseWriter, r *http.Request) {
	v, err := s.Svc.GetDomain(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (s *Server) patchDomain(w http.ResponseWriter, r *http.Request) {
	var in struct {
		CatchAll       *bool  `json:"catch_all"`
		CatchAllTarget string `json:"catch_all_target"`
	}
	if e := decode(r, &in); e != nil {
		writeErr(w, e)
		return
	}
	if in.CatchAll == nil {
		writeErr(w, service.Bad("catch_all is required", nil))
		return
	}
	v, err := s.Svc.PatchCatchAll(r.Context(), r.PathValue("name"), *in.CatchAll, in.CatchAllTarget)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (s *Server) deleteDomain(w http.ResponseWriter, r *http.Request) {
	res, err := s.Svc.DeleteDomain(r.Context(), r.PathValue("name"), r.URL.Query().Get("force") == "true")
	if err != nil {
		s.fail(w, err)
		return
	}
	if res.Done {
		w.WriteHeader(204)
		return
	}
	writeJSON(w, 202, map[string]any{"status": "deleting", "job_id": res.JobID,
		"message": "domain is marked deleted; cleanup in Stalwart/DNS is queued and will retry"})
}

func (s *Server) domainStatus(w http.ResponseWriter, r *http.Request) {
	v, err := s.Svc.StatusOf(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (s *Server) dnsRecords(w http.ResponseWriter, r *http.Request) {
	d, err := s.Svc.GetDomainEntity(r.Context(), r.PathValue("name"))
	if err != nil {
		s.fail(w, err)
		return
	}
	recs, err := s.Svc.DNSRecords(r.Context(), d)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"domain": d.Name, "records": recs})
}

// ---------- mailboxes ----------

func (s *Server) createMailbox(w http.ResponseWriter, r *http.Request) {
	var in struct {
		LocalPart  string `json:"local_part"`
		QuotaBytes int64  `json:"quota_bytes"`
		Password   string `json:"password"`
	}
	if e := decode(r, &in); e != nil {
		writeErr(w, e)
		return
	}
	v, err := s.Svc.CreateMailbox(r.Context(), r.PathValue("domain"), in.LocalPart, in.QuotaBytes, in.Password)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 201, v)
}

func (s *Server) listMailboxes(w http.ResponseWriter, r *http.Request) {
	v, err := s.Svc.ListMailboxes(r.Context(), r.PathValue("domain"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"mailboxes": v})
}

func (s *Server) getMailbox(w http.ResponseWriter, r *http.Request) {
	v, err := s.Svc.GetMailbox(r.Context(), r.PathValue("address"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (s *Server) patchMailbox(w http.ResponseWriter, r *http.Request) {
	var in struct {
		QuotaBytes int64 `json:"quota_bytes"`
	}
	if e := decode(r, &in); e != nil {
		writeErr(w, e)
		return
	}
	v, err := s.Svc.PatchQuota(r.Context(), r.PathValue("address"), in.QuotaBytes)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func (s *Server) resetPassword(w http.ResponseWriter, r *http.Request) {
	pw, err := s.Svc.ResetPassword(r.Context(), r.PathValue("address"))
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"new_password": pw})
}

func (s *Server) deleteMailbox(w http.ResponseWriter, r *http.Request) {
	res, err := s.Svc.DeleteMailbox(r.Context(), r.PathValue("address"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if res.Done {
		writeJSON(w, 200, map[string]any{"deleted_messages_count": res.DeletedMessages})
		return
	}
	writeJSON(w, 202, map[string]any{"status": "deleting", "job_id": res.JobID,
		"message": "mailbox is marked deleted; removal in Stalwart is queued and will retry"})
}

// ---------- aliases ----------

func (s *Server) createAlias(w http.ResponseWriter, r *http.Request) {
	var in struct {
		LocalPart string   `json:"local_part"`
		Targets   []string `json:"targets"`
	}
	if e := decode(r, &in); e != nil {
		writeErr(w, e)
		return
	}
	v, err := s.Svc.CreateAlias(r.Context(), r.PathValue("domain"), in.LocalPart, in.Targets)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 201, v)
}

func (s *Server) listAliases(w http.ResponseWriter, r *http.Request) {
	v, err := s.Svc.ListAliases(r.Context(), r.PathValue("domain"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"aliases": v})
}

func (s *Server) deleteAlias(w http.ResponseWriter, r *http.Request) {
	if err := s.Svc.DeleteAlias(r.Context(), r.PathValue("domain"), r.PathValue("local_part")); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(204)
}

// ---------- usage ----------

func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	v, err := s.Svc.Usage(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, v)
}
