# Arsitektur iconics-eos-monitoring

Sistem monitoring SCADATR dengan distribusi WhatsApp otomatis dan dashboard
internal. Satu stack di Portainer; host operator hanya dipakai untuk
menyegarkan token portal.

## Komponen

```text
HOST (laptop operator)                  STACK (Portainer)
+-------------------------------+       +---------------------------------+
| scada-token-refresh.timer     |       | monitoring-api (Go)             |
|  07:20 & 16:30 WIB            |  PUT  |  - REST API (chi)               |
|  -> scripts/push_token.py     |  /api/|  - SQLite (volume)              |
|     (extract_token.py dari    |  session  |  - scheduler cron            |
|      profil Chromium)         |  /token|    07:40 & 16:50 WIB          |
+-------------------------------+------>|  - portalclient: REST + socket  |
                                        |    io headless (tanpa Chromium) |
                                        |  - pengirim WhatsApp (stagger)  |
                                        +---------------+-----------------+
                                                        |
                                                        v
                                        +---------------------------------+
                                        | whatsapp-service (Node/Baileys) |
                                        +---------------------------------+
                                        +---------------------------------+
                                        | frontend static (:5117)         |
                                        +---------------------------------+
```

`internal/portal` (scraper chromedp) dan `cmd/collector` masih ada sebagai
cadangan (`PORTAL_MODE=browser`), bukan jalur harian.

## Sumber data

1. **Prometheus `http://192.168.3.204:9090`** - metrik server rocky-server.
   Query sama dengan laporan resmi: uptime, RAM total, CPU cores, CPU busy,
   sys load (dinormalisasi per core), RAM used, swap used, container running.

2. **Portal `https://scadatr.grita.id`** - status telemetri RTU, dibaca
   headless dengan token JWT sebagai header `Authorization: Bearer`.
   - Daftar RTU (REST, port 14000):
     `POST /master-rtu/dropdown-rtu-by-protocol` body `{"protocol":"MQTT"}` ->
     `{statusCode, message, data:[{id_rtu, protocol, station:{name_station}}]}`.
   - Daftar klien + status broker (socket.io, port 14013, engine.io v4 lewat
     HTTP polling): `GET /socket.io/?EIO=4&transport=polling` -> `0{sid,...}`;
     `POST ...&sid=<sid>` body `40` (CONNECT); `POST` body `42["data-mqtt"]`
     (emit); lalu `GET` berulang. Pesan dipisah byte `0x1e`; `2` = ping
     (dibalas `3`); `42[...]` = `[event, payload]`. Event `mqtt` berisi 22
     klien dengan `id_rtu`, `name`, `host`, `port`, `status_mqtt`,
     `state_mqtt`, `rtu_online_at`, `rtu_offline_at`, `station`.
   - Traffic per RTU: event `log-mqtt` (`time` `dd/mm/yyyy HH:MM:SS.mmm`,
     `name`, `id_rtu`, `topic`, `logging.timestamp`) dan `datapoint-mqtt`
     (`id_rtu`, `type`, `value`, `quality`).
   - Peta service dari bundle SPA: `SYSTEM` 14000, `ALARM` 14001, `TABULAR`
     14002, `MQTT` 14013, `MONITORING` 14015, `LOGS` 14999, `WS_KAFKA`
     `wss://<host>:30100` (websocket-only; menolak polling dengan HTTP 400 dan
     tidak mengirim event tanpa langganan).
   - Panel `Traffic & Log` di UI dirender murni dari event socket yang sudah
     terkumpul di memori tab (klik RTU tidak memicu request apa pun), sehingga
     isinya bergantung pada berapa lama tab dibuka - ini alasan pembacaan
     headless memakai jendela waktu, bukan buffer sesi.

## Aliran data

1. Scheduler cron di `monitoring-api` memicu siklus pada 07:40 dan 16:50 WIB
   (pemicu; laporan terkirim sekitar 10 menit kemudian bila ada RTU yang diam).
2. Siklus mengambil metrik Prometheus dan membaca portal lewat
   `internal/portalclient`:
   - `FetchClients` (emit `data-mqtt`, tunggu event `mqtt`, ~0,3-0,5 detik),
     dengan cadangan REST `master-rtu/dropdown-rtu-by-protocol` tanpa status
     broker bila event tidak datang.
   - `CollectLogs` mengumpulkan `log-mqtt`/`datapoint-mqtt` selama jendela
     adaptif: minimum 60 detik, berhenti lebih awal bila semua RTU Connected
     sudah mengirim, maksimum 600 detik.
3. Laporan dirender format resmi, disimpan ke `report_history`, dan dikirim ke
   semua recipient aktif via whatsapp-service (satu pesan per penerima,
   stagger minimal 2 detik).
4. Frontend membaca REST API: laporan terakhir, riwayat, status sesi portal,
   status koneksi WA.

## Penjadwalan dan anti-duplikasi

- Jadwal harian dijalankan cron internal container:
  `DISABLE_INTERNAL_SCHEDULER=0`, `SKIP_PORTAL=0`, `PORTAL_MODE=socket`.
- Jalur lama sudah dipensiunkan: systemd timer `monitoring-report-host.timer`
  nonaktif, `scripts/run_report_host.py` tidak dipakai, dan cron Hermes
  `scada_check.sh` (07:40 & 16:50) di-pause. Keduanya masih bisa dihidupkan
  kembali sebagai cadangan; bila dihidupkan, keduanya berbagi lock
  `/tmp/monitoring-report-host.lock` supaya tidak ada dua sweep bersamaan.
- Durasi siklus: ~60 detik bila semua RTU mengirim; sampai 10 menit bila ada
  RTU yang tidak mengirim sama sekali (jendela maksimum), karena RTU yang
  lambat tidak boleh langsung dianggap "no data".

## Aturan klasifikasi telemetri RTU

- `Active`: `status_mqtt` true (`Connected`) dan ada minimal satu event
  `log-mqtt`/`datapoint-mqtt` untuk `id_rtu` tersebut di dalam jendela.
- `no data from broker`: selebihnya, termasuk broker yang tidak `Connected`.
- Timestamp traffic terakhir tetap dibaca dari panel dan dipakai untuk log serta
  progres pengumpulan (`rtu_terkumpul`/`rtu_diharapkan` pada
  `GET /api/report/status`), tetapi tidak dimasukkan ke isi laporan.

## Manajemen sesi portal

- Token JWT portal berumur sekitar 8 jam dan disimpan di SQLite
  (`portal_session`), diisi lewat `PUT /api/session/token`.
- `scripts/push_token.py` mengekstrak token dari profil Chromium operator
  (`~/.cache/scada-chromium-1`) dan mengirimkannya ke API; dijalankan otomatis
  oleh systemd user timer `scada-token-refresh.timer` pada 07:20 dan 16:30 WIB
  (20 menit sebelum pemicu laporan 07:40 & 16:50). Token yang tidak lebih baru
  daripada yang sedang dipakai API tidak didorong, supaya sesi yang masih sah
  tidak tertimpa token lama (`--force` untuk memaksa).
- `scripts/token_watch.py` (systemd user service `scada-token-watch.service`)
  memeriksa profil tiap 20 detik dan hanya mendorong token yang `exp`-nya lebih
  baru daripada token di API, sehingga login operator kapan pun langsung
  terpakai tanpa langkah manual dan token lama tidak menimpa token yang sah.
  `GET /api/session/status` menyertakan `token_exp` sebagai pembandingnya.
- Bila portal menolak token (401/403), siklus menandai sesi mati, laporan
  disimpan tanpa bagian telemetri, dan pengirimannya ditahan kecuali
  `SEND_WITHOUT_TELEMETRY=1`, sehingga tidak ada angka yang dikarang.
- Pemulihan sesi profil: login ulang sekali di jendela Chromium profil otomasi;
  token barunya didorong otomatis oleh `scada-token-watch.service` (manual:
  `python3 scripts/push_token.py`).

## Keamanan

- Token JWT portal hanya ada di profil Chromium operator dan volume SQLite;
  sesi Baileys hanya di volume Docker.
- whatsapp-service tidak expose ke jaringan luar; hanya API dan frontend yang
  publish port host.
- Endpoint mutasi REST dilindungi token statis dari `.env` (internal use).

## Port (host)

- API: 5118
- frontend: 5117
- whatsapp-service: 3101 (untuk scan QR awal dan health)

## Batas yang diketahui

- RTU yang memang tidak mengirim apa pun selama jendela akan masuk
  `no data from broker` walau `status_mqtt` true. Pada 23/09/2026 ada 8 RTU
  seperti itu (UPS.01.FUY, UPS.02.DUY, UPS.08.IUY, UPS.11.DUY, UPS.02.FUY,
  UPS.08.FUY, UPS.01.CUY, UPS.13.DUY).
- Sesi portal bisa mati sewaktu-waktu (single session di sisi portal);
  pemulihannya login ulang di profil otomasi lalu jalankan `push_token.py`.
- Entrypoint container masih menyalakan Xvfb untuk cadangan `PORTAL_MODE=browser`.
- Portal tidak menyediakan endpoint riwayat log, jadi definisi "punya data"
  selalu berarti "mengirim di dalam jendela", bukan "pernah mengirim".
