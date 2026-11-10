# SelfMail API

Layanan provisioning email self-hosted (backend-only) di atas [Stalwart](https://stalw.art).
Satu binary Go (API + worker), satu PostgreSQL. SMTP/IMAP/DKIM/spam/kuota didelegasikan penuh ke Stalwart.
Spesifikasi: `docs/PRD  Product Requirements Document-2026-11-10.md`.

## Menjalankan

```bash
cp .env.example .env           # isi STALWART_*, MAIL_HOST, (opsional) CLOUDFLARE_API_TOKEN
docker compose up -d --build
docker compose exec selfmail /selfmail token create admin-laptop   # token tampil sekali
curl -H "Authorization: Bearer $TOKEN" localhost:8080/domains
```

Tanpa Docker: `go run ./cmd/selfmail` (butuh `DATABASE_URL`, `STALWART_URL`, `MAIL_HOST`; migrasi otomatis).
Konfigurasi tidak valid (mis. threshold negatif) membuat proses gagal start dengan pesan jelas.

## Endpoint (semua kecuali "publik" butuh `Authorization: Bearer <token>`)

| Metode & path | Fungsi |
|---|---|
| `POST /domains` `{name}` | Daftarkan domain → 201 + `dns_status` + `dns_records[]`; enqueue job DNS |
| `GET /domains`, `GET /domains/{name}` | Detail + agregat pemakaian |
| `PATCH /domains/{name}` `{catch_all, catch_all_target?}` | Catch-all |
| `DELETE /domains/{name}[?force=true]` | 204; 409 + daftar mailbox bila belum kosong; 202 bila cleanup ditunda |
| `GET /domains/{name}/status`, `POST /domains/{name}/verify` | Query DNS **aktual**, status per record |
| `GET /domains/{name}/dns-records` | Record yang harus dibuat manual |
| `POST/GET /domains/{d}/mailboxes` | Buat (password acak dikembalikan **sekali**) / daftar |
| `GET/PATCH/DELETE /mailboxes/{address}` | Detail+usage / ubah kuota / hapus (`deleted_messages_count`) |
| `POST /mailboxes/{address}/password/reset` | Password baru (sekali) |
| `POST/GET /domains/{d}/aliases`, `DELETE /domains/{d}/aliases/{lp}` | Alias 1–5 target mailbox |
| `GET /usage` | Snapshot instalasi + top-10 mailbox |
| `GET /healthz`, `GET /readyz` | Publik. `readyz` 503 bila DB/Stalwart mati |
| `GET /mail/config-v1.1.xml`, `/.well-known/autoconfig/mail/config-v1.1.xml` | Publik, autoconfig Thunderbird |

Error selalu `{"error":{"code","message","details?","retry_after?"}}`; 503 menyertakan header `Retry-After`.

## Perilaku kunci

- **DNS jujur / fail-closed**: `verified` hanya jika MX, SPF, DKIM, DMARC semuanya `ok` dari query langsung ke 1.1.1.1/8.8.8.8 (timeout 5s/record). Resolver gagal → `error`; nilai beda → `mismatch` dan **tidak pernah ditimpa**.
  Agregat: semua ok→`verified`; ada mismatch→`failed`; ada error→`error`; sebagian ok→`propagating`; semua missing→`pending`.
- `dns_status` di respons mailbox/alias/usage memakai cache yang sama (refresh ≤ 5 menit).
- Usage dari Stalwart, cache 60 detik; Stalwart tak terjangkau dan cache kedaluwarsa → 503 (bukan 0).
- Password plaintext tidak pernah disimpan atau di-log; audit log tidak memuat password.
- Job queue di Postgres (`FOR UPDATE SKIP LOCKED`, backoff eksponensial, recovery job macet) + job `reconcile` tiap 5 menit untuk crash recovery (soft delete → hapus di Stalwart → hard delete).

## Test

```bash
go test ./...                                              # unit
TEST_DATABASE_URL=postgres://... go test ./...             # + end-to-end (Postgres asli, Stalwart/CF/DNS palsu)
```
Database di `TEST_DATABASE_URL` akan **dikosongkan** (`DROP SCHEMA public`) — pakai DB khusus test.

## Belum dikerjakan / catatan jujur

- **Belum diuji terhadap Stalwart asli.** Klien (`internal/stalwart`) memakai API manajemen `/api/principal`, `/api/dkim`, `/api/dns/records`. Dokumentasi Stalwart terbaru beralih ke JMAP (`x:Account/set`); sesuaikan klien itu bila versi Anda berbeda (semua dependensi lewat interface `stalwart.API`). Kunci setting di `ApplyStalwartSettings` (threshold spam, ukuran pesan) juga perlu dicocokkan dengan versi Stalwart Anda.
- **Belum ada**: FR-09 (feedback spam Junk↔Inbox; butuh pemantauan IMAP, tabel `spam_feedbacks` sudah ada), FR-16/FR-25 (cermin `outbound_queue` & DSN — retry/bounce saat ini ditangani Stalwart sendiri), `/metrics`, OpenAPI.
- FR-05/07/08/24 (inbound, submission, IMAP, header spam) adalah perilaku Stalwart; yang dilakukan di sini hanya mendorong konfigurasi.
- Kunci privat DKIM dibuat dan disimpan Stalwart; kolom `dkim_private_key` tidak dipakai (hanya public key disimpan).
- Penyimpangan skema dari PRD: kolom `dns_records.purpose` (mx/spf/dkim/dmarc), job type `reconcile`, `timestamptz` alih-alih `timestamp`.
