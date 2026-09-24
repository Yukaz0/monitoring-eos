# iconics-eos-monitoring

Sistem monitoring SCADATR dengan distribusi WhatsApp otomatis. Desain lengkap
ada di `docs/arsitektur.md` (source of truth).

## Komponen

| Komponen | Bahasa | Fungsi |
|---|---|---|
| `cmd/api` | Go | REST API (chi) + scheduler cron + siklus laporan |
| `cmd/collector` | Go | CLI satu siklus penuh: metrik + pembacaan portal + render + kirim WA |
| `internal/prometheus` | Go | Client `/api/v1/query` Prometheus 192.168.3.204 |
| `internal/portalclient` | Go | Pembaca portal headless: socket.io HTTP polling + REST (tanpa browser) |
| `internal/portal` | Go | Scraper chromedp (cadangan, hanya bila `PORTAL_MODE=browser`) |
| `internal/report` | Go | Renderer laporan format resmi "Laporan Harian SCADATR UP2D JAYA" |
| `internal/store` | Go | SQLite CGO-free (modernc.org/sqlite) |
| `internal/api` | Go | Router chi + pengirim WhatsApp (stagger 2 detik) |
| `internal/scheduler` | Go | robfig/cron v3, zona Asia/Jakarta |
| `internal/cycle` | Go | Perakit siklus: prometheus + portal + report + store + WA |
| `apps/whatsapp-service` | Node | Gateway Baileys (diadaptasi dari smsgateway-grita) |
| `web/` | HTML | Placeholder dashboard (dashboard menyusul) |
| `scripts/extract_token.py` | Python | Ekstrak token portal dari LevelDB profil Chromium operator |
| `scripts/push_token.py` | Python | Kirim token itu ke API (`PUT /api/session/token`) |

## Penjadwalan laporan harian (07:40 dan 16:00 WIB)

Sweep portal dijalankan **di dalam container tanpa browser**: `internal/portalclient`
memakai REST + socket.io portal (engine.io HTTP polling) dengan token JWT sebagai
header `Authorization: Bearer`. Chromium/headful tidak dipakai lagi.

```text
scheduler cron container (07:40 & 16:00 WIB - pemicu; laporan sampai ~10 menit kemudian)
  -> siklus: metrik Prometheus + pembacaan portal headless
  -> laporan dirender (format resmi) dan disimpan ke report_history
  -> dikirim ke semua recipient aktif via whatsapp-service (stagger 2 detik)
```

Env yang relevan di `.env`:

- `PORTAL_MODE=socket` - jalur utama tanpa browser (`browser` = jalur chromedp lama).
- `SKIP_PORTAL=0` - portal ikut dibaca.
- `DISABLE_INTERNAL_SCHEDULER=0` - jadwal dijalankan cron container.
- `PORTAL_LOG_WINDOW_SECONDS=300` - jendela maksimum pengumpulan log (5 menit,
  batas keras).
- `PORTAL_LOG_MIN_WINDOW_SECONDS=60` - jendela minimum; berhenti lebih awal begitu
  semua RTU yang broker-nya Connected sudah mengirim minimal satu event.
- `PORTAL_LOG_IDLE_SECONDS=120` - berhenti lebih awal juga bila tidak ada RTU
  baru mengirim selama ini (dihitung sejak RTU terakhir yang bertambah). Tanpa
  ini, RTU kronis yang tidak pernah mengirim memaksa setiap siklus menunggu
  jendela maksimum penuh. `0` mematikan.
- `PORTAL_HOST`, `PORTAL_MQTT_PORT=14013`, `PORTAL_SYSTEM_PORT=14000`.
- `PORTAL_LOGIN_ENABLED=1` dengan `PORTAL_USERNAME`, `PORTAL_PASSWORD`, dan
  `PORTAL_TOTP_SECRET` - siklus login sendiri ke portal (password + kode TOTP
  yang dihitung lokal, RFC 6238) bila token tersimpan ditolak. Ini jalur utama,
  tidak butuh browser dan tidak butuh layanan TOTP pihak ketiga.
- `PORTAL_LOGIN_COOLDOWN_MINUTES=360` - jeda setelah login gagal supaya akun
  portal tidak terkunci karena percobaan berulang.

Token portal hanya berlaku sekitar 8 jam dan diperbarui otomatis oleh siklus
(login mandiri dengan TOTP). Timer user di host (`scada-token-refresh.timer`)
dan `scada-token-watch.service` adalah jalur cadangan dari masa sebelum login
otomatis tersedia (lihat "Alur pembaruan token portal"); pada deployment di
server keduanya tidak dipakai.

```sh
systemctl --user enable --now scada-token-refresh.timer   # 07:20 & 16:30 WIB
systemctl --user enable --now scada-token-watch.service   # pantau tiap 20 detik
systemctl --user list-timers scada-token-refresh.timer
```

Jalur lama (sweep Chromium di host) sudah dipensiunkan: timer
`monitoring-report-host.timer` nonaktif, `scripts/run_report_host.py` tidak
dipakai lagi, dan cron lama di Hermes (`scada_check.sh`) di-pause. File-file itu
masih ada sebagai rujukan bila sewaktu-waktu diperlukan.

## Build dan test

```sh
go build ./...
go vet ./...
go test ./...
```

## Menjalankan stack

```sh
cp .env.example .env   # isi API_KEY, WA_API_KEY, dst.
docker compose up -d --build
```

Port host: API 5118, whatsapp-service 3101 (hanya localhost), frontend 5117.

Deploy di server SCADATR (`192.168.3.204`, tempat portal dan Prometheus
berjalan) dijelaskan terpisah di `docs/deploy-server-scadatr.md`: ada dua
variabel `.env` yang berbeda dan langkah verifikasi jalur container ke
portal/Prometheus.

## API

Endpoint mutasi (POST/PUT/DELETE, `POST /api/report/run`,
`POST /api/report/submit`) wajib header `X-API-Key` cocok dengan env `API_KEY`.

- `GET /api/health`
- `GET/POST/PUT/DELETE /api/recipients`
- `GET/PUT /api/schedules` (PUT memuat ulang cron scheduler)
- `GET /api/report/latest`, `GET /api/report/history?limit=N`
- `POST /api/report/run` - picu siklus manual (async, 202)
- `POST /api/report/submit` - simpan laporan jadi lalu teruskan ke WhatsApp
- `GET /api/session/status`, `PUT /api/session/token` - sesi portal
- `GET /api/wa/status` - status koneksi WhatsApp
- `GET /api/qr` - gambar QR pairing (PNG)
- `POST /api/wa/logout` - logout sesi WhatsApp, QR baru muncul

## Aturan klasifikasi telemetri RTU

- `Lambung Active`: status broker (`status_mqtt`) `Connected` **dan** ada
  minimal satu event `log-mqtt`/`datapoint-mqtt` untuk RTU itu di dalam jendela
  pengumpulan.
- `Lambung no data from broker`: selebihnya, termasuk broker yang tidak
  `Connected`.
- Timestamp traffic terakhir tetap dibaca dari panel dan dipakai untuk log serta
  progres pengumpulan, tetapi tidak dimasukkan ke isi laporan.

## Alur pembaruan token portal

Jalur utama: otomatis di dalam container. Bila token tersimpan ditolak portal
(401/403), siklus login sendiri memakai `PORTAL_USERNAME`, `PORTAL_PASSWORD`,
dan `PORTAL_TOTP_SECRET` (`PORTAL_LOGIN_ENABLED=1`), lalu menyimpan token baru
ke SQLite. Langkah di bawah ini adalah jalur cadangan bila login otomatis
dinonaktifkan atau akun otomasi belum siap.

1. Login portal di profil Chromium operator (`~/.cache/scada-chromium-1`) -
   hanya perlu dilakukan saat sesi profil itu mati.
2. `python3 scripts/push_token.py` - mengekstrak token dari profil lalu
   mengirimnya ke API (`PUT /api/session/token`). Isi token tidak pernah
   dicetak ke log.
3. Siklus berikutnya memakai token dari SQLite. Bila portal menolak token
   (401/403), siklus menandai sesi mati, laporan disimpan tanpa bagian
   telemetri, dan pengirimannya ditahan (kecuali `SEND_WITHOUT_TELEMETRY=1`).
4. Penyegaran cadangan: `scada-token-refresh.timer` menjalankan langkah 2 pada
   07:20 dan 16:30 WIB (dibuat untuk jadwal lama 07:40 dan 16:50). Pada
   deployment dengan login otomatis aktif, timer ini tidak diperlukan dan
   sebaiknya dinonaktifkan supaya tidak berebut sesi dengan akun yang sama.
5. Pemantauan berkelanjutan: `scada-token-watch.service` menjalankan
   `scripts/token_watch.py` (periksa tiap 20 detik) dan mendorong token begitu
   nilainya lebih baru daripada yang sedang dipakai API - jadi login di langkah
   1 langsung terpakai tanpa menjalankan langkah 2 manual. Token yang lebih lama
   tidak pernah menimpa token yang masih sah.

## Catatan

- Nomor penerima boleh ditulis dalam format lokal (`085161971784`) atau
  internasional (`6285161971784`); whatsapp-service mengonversi awalan `0`
  menjadi `62` sebelum membentuk JID. Tanpa konversi itu, `sendMessage`
  menggantung sampai batas waktu dan laporan gagal terkirim.
- whatsapp-service wajib mengisi `getMessage` pada `makeWASocket` dan menyimpan
  isi pesan yang baru dikirim: tanpa itu retry receipt dari penerima tidak
  terjawab dan pesan tampil "Waiting for this message. This may take a while."
  di aplikasi WhatsApp penerima walau pengiriman kita terlihat sukses. Status
  ack (`server` → `diantar` → `dibaca`) dicatat di log untuk memantau ini.
- Saat bagian telemetri RTU gagal dibaca (sesi portal mati), laporan tetap
  disimpan ke riwayat tetapi **tidak dikirim**; pengiriman hanya bisa
  dilonggarkan sementara lewat `SEND_WITHOUT_TELEMETRY=1`.
- Pembacaan portal headless terukur jauh lebih cepat daripada sweep browser
  (fetch klien ~0,3-9 detik; pengumpulan log 60 detik bila semua RTU aktif, dan
  berhenti lebih awal begitu tidak ada RTU baru mengirim selama
  `PORTAL_LOG_IDLE_SECONDS`, paling lama 5 menit).
- Diverifikasi: `go build ./...`, `go vet ./...`, `go test ./...` sukses.
