# SelfMail API

Layanan provisioning email self-hosted (backend-only) di atas [Stalwart](https://stalw.art).
Satu binary Go (API + worker), satu PostgreSQL. SMTP/IMAP/DKIM/spam/kuota didelegasikan penuh ke Stalwart.
Spesifikasi: `docs/PRD  Product Requirements Document-2026-11-10.md`.

## Menjalankan

```bash
cp .env.example .env
export STALWART_RECOVERY_ADMIN=admin:ganti-password-ini
docker compose up -d postgres stalwart

# Stalwart (>= 1.x) baru berjalan dalam "bootstrap mode": setup sekali, lalu restart.
STALWART_URL=http://localhost:8081 MAIL_HOST=mail.example.com DEFAULT_DOMAIN=example.com \
  ./scripts/bootstrap-stalwart.sh        # mencetak STALWART_USER / STALWART_PASSWORD -> taruh di .env
docker compose restart stalwart

docker compose up -d --build selfmail
docker compose exec selfmail /selfmail token create admin-laptop   # token tampil sekali
curl -H "Authorization: Bearer $TOKEN" localhost:8080/domains
```
Gunakan `TLS=true` pada skrip bootstrap di server publik agar Stalwart meminta sertifikat Let's Encrypt.

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
STALWART_TEST_URL=http://localhost:8081 STALWART_TEST_USER=... STALWART_TEST_PASS=... go test ./internal/stalwart   # klien vs Stalwart asli
```
Database di `TEST_DATABASE_URL` akan **dikosongkan** (`DROP SCHEMA public`) — pakai DB khusus test.

## Status verifikasi (Stalwart v1.1.0, lokal)

Klien memakai API manajemen JMAP Stalwart (`x:Domain`, `x:Account`, `x:MailingList`, `x:DkimSignature`) dan **sudah diuji terhadap Stalwart v1.1.0 asli**:
buat/hapus domain, DKIM (selector & kunci publik dibaca dari zone file Stalwart), mailbox, kuota, password, usage, alias multi-target, catch-all.
Diverifikasi juga lewat SMTP/IMAP sungguhan: email ke mailbox, alias (fan-out), dan alamat tak terdaftar (catch-all) sampai; lima folder standar otomatis ada; header `Authentication-Results` dan `X-Spam-*` hadir; reset password mencabut password lama.

Temuan perilaku Stalwart yang perlu diketahui:
- **Kuota tidak ditolak saat SMTP.** Stalwart membalas `250 Message queued`, lalu pengiriman lokal gagal sementara (`451 4.3.0 Mailbox over quota`) dan pesan tetap di antrean untuk di-retry, bukan `452 4.2.2` seketika seperti di PRD (FR-05d). Pesan tidak hilang, tetapi perilakunya berbeda dari spesifikasi.
- Selector DKIM dibuat Stalwart (mis. `v1-rsa-20261110`), bukan `default`; app membaca selector dari Stalwart.
- Catch-all hanya mendukung **satu** target (field `catchAllAddress` pada domain).
- Stalwart menolak beberapa nama domain (mis. TLD `.example`); `.test` dan `.com` diterima.
- Email dari pengirim palsu di tes lokal selalu masuk Junk (skor ~15, SPF/rDNS gagal) — itu juga membuktikan skor tinggi disimpan di Junk, tidak dibuang.

## Belum dikerjakan / catatan jujur

- **Threshold spam, ukuran pesan, rate limit, retry/expiry antrean (FR-06/FR-22 sebagian) belum didorong ke Stalwart.** Variabel `SPAM_*`, `MAX_MESSAGE_SIZE`, dst. sudah divalidasi tetapi belum diterapkan; di Stalwart v1.1 pengaturan itu berupa objek registry, bukan key-value, dan belum dipetakan.
- **Belum ada**: FR-09 (feedback spam Junk↔Inbox; butuh pemantauan IMAP, tabel `spam_feedbacks` sudah ada), FR-16/FR-25 (cermin `outbound_queue` & DSN — retry/bounce ditangani Stalwart sendiri), `/metrics`, OpenAPI.
- **Belum diuji**: relay keluar (Resend), pengiriman ke/dari Gmail, DNS/Cloudflare asli, DKIM `pass` di penerima — butuh domain dan server publik.
- Nilai SPF/DMARC yang diharapkan app (`v=spf1 ip4:... -all`, `p=quarantine`) sengaja berbeda dari zone file default Stalwart (`mx -all`, `p=reject`).
- Kunci privat DKIM dibuat dan disimpan Stalwart; kolom `dkim_private_key` tidak dipakai (hanya public key disimpan).
- Penyimpangan skema dari PRD: kolom `dns_records.purpose` (mx/spf/dkim/dmarc), job type `reconcile`, `timestamptz` alih-alih `timestamp`.
