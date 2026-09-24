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

## Penjadwalan laporan harian (07:40 dan 16:50 WIB)

Sweep portal dijalankan **di dalam container tanpa browser**: `internal/portalclient`
memakai REST + socket.io portal (engine.io HTTP polling) dengan token JWT sebagai
header `Authorization: Bearer`. Chromium/headful tidak dipakai lagi.

```text
scheduler cron container (07:40 & 16:50 WIB - pemicu; laporan sampai ~10 menit kemudian)
  -> siklus: metrik Prometheus + pembacaan portal headless
  -> laporan dirender (format resmi) dan disimpan ke report_history
  -> dikirim ke semua recipient aktif via whatsapp-service (stagger 2 detik)
```

Env yang relevan di `.env`:

- `PORTAL_MODE=socket` - jalur utama tanpa browser (`browser` = jalur chromedp lama).
- `SKIP_PORTAL=0` - portal ikut dibaca.
- `DISABLE_INTERNAL_SCHEDULER=0` - jadwal dijalankan cron container.
- `PORTAL_LOG_WINDOW_SECONDS=600` - jendela maksimum pengumpulan log (10 menit).
- `PORTAL_LOG_MIN_WINDOW_SECONDS=60` - jendela minimum; berhenti lebih awal begitu
  semua RTU yang broker-nya Connected sudah mengirim minimal satu event.
- `PORTAL_HOST`, `PORTAL_MQTT_PORT=14013`, `PORTAL_SYSTEM_PORT=14000`.

Token portal hanya berlaku sekitar 8 jam. Timer user di host menyegarkannya
sebelum jadwal laporan (20 menit sebelum pemicu), dan
`scada-token-watch.service` mendorong token baru begitu operator login
(lihat "Alur pembaruan token portal"):

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

Port host: API 5118, whatsapp-service 3101, frontend 5117.

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

1. Login portal di profil Chromium operator (`~/.cache/scada-chromium-1`) -
   hanya perlu dilakukan saat sesi profil itu mati.
2. `python3 scripts/push_token.py` - mengekstrak token dari profil lalu
   mengirimnya ke API (`PUT /api/session/token`). Isi token tidak pernah
   dicetak ke log.
3. Siklus berikutnya memakai token dari SQLite. Bila portal menolak token
   (401/403), siklus menandai sesi mati, laporan disimpan tanpa bagian
   telemetri, dan pengirimannya ditahan (kecuali `SEND_WITHOUT_TELEMETRY=1`).
4. Penyegaran otomatis: `scada-token-refresh.timer` menjalankan langkah 2 pada
   07:20 dan 16:30 WIB - 20 menit sebelum pemicu laporan 07:40 dan 16:50.
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
  (fetch klien ~0,3-0,5 detik; pengumpulan log 60 detik bila semua RTU aktif,
  sampai 10 menit bila ada RTU yang diam).
- Diverifikasi: `go build ./...`, `go vet ./...`, `go test ./...` sukses.
