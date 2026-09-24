# Deploy di server SCADATR (192.168.3.204)

Stack ini bisa dijalankan langsung di server `192.168.3.204` — mesin yang juga
menjalankan Prometheus (`:9090`) dan portal `scadatr.grita.id` (`:14000` REST,
`:14013` socket.io). Di sana kedua sumber data jadi lokal, jadi Tailscale tidak
dipakai lagi dan pembacaan portal tidak lagi bergantung pada jaringan luar.

Prasyarat: Docker + Docker Compose (plugin `docker compose`), akses Docker Hub
untuk build, arsitektur `x86_64` (image dibangun untuk amd64), serta port
`5118` (API + dashboard), `5117` (frontend nginx), dan `3101` (whatsapp-service,
hanya localhost) yang bebas.

## 1. Yang berbeda dari deployment laptop

Hanya dua variabel/berkas, bukan kode:

| Berkas | Laptop | Server 192.168.3.204 |
|---|---|---|
| `.env` → `PROMETHEUS_URL` | `http://192.168.3.204:9090` | `http://host.docker.internal:9090` |
| `.env` → `API_KEY`, `WA_API_KEY` | nilai dev | **wajib diganti nilai acak baru** |
| `scada-token-refresh.timer`, `scada-token-watch.service` (systemd user) | dipakai jalur cadangan | tidak dipakai (login portal otomatis) |

`extra_hosts` di `docker-compose.yml` sudah memuat dua baris:
`scadatr.grita.id:192.168.3.204` (portal di host ini) dan
`host.docker.internal:host-gateway` (jalur container ke host untuk Prometheus).
Jangan mengubah `PROMETHEUS_URL` di `.env` laptop menjadi
`host.docker.internal` — di laptop nama itu menunjuk ke laptop sendiri dan
Prometheus tidak ada di sana.

## 2. Isi `.env`

```sh
cd /opt/iconics-eos-monitoring
cp .env.example .env
```

Nilai yang harus diisi atau disesuaikan:

```sh
PROMETHEUS_URL=http://host.docker.internal:9090
API_KEY=<nilai acak, mis. openssl rand -hex 24>
WA_API_KEY=<nilai acak, mis. openssl rand -hex 24>
PORTAL_LOGIN_ENABLED=1
PORTAL_USERNAME=<akun otomasi portal>
PORTAL_PASSWORD=<password akun itu>
PORTAL_TOTP_SECRET=<secret base32, 52 karakter>
PORTAL_HOST=scadatr.grita.id
PORTAL_MQTT_PORT=14013
PORTAL_SYSTEM_PORT=14000
```

Sisa variabel (`SQLITE_DSN`, `LISTEN_ADDR`, `PORTAL_MODE=socket`,
`PORTAL_LOG_WINDOW_SECONDS`, `WA_BASE_URL`, `WA_AUTH_DIR`, dst.) boleh disalin
apa adanya. `.env` tidak pernah masuk git — sudah ada di `.gitignore`.

## 3. Build dan jalankan

```sh
docker compose up -d --build
docker compose ps
curl -s http://127.0.0.1:5118/api/health
```

## 4. Uji jalur container ke dua sumber data

Langkah paling sering gagal di server berbasis firewalld (Rocky Linux):
container tidak diizinkan menghubungi port milik host.

```sh
docker compose exec monitoring-api sh -c \
  'nc -vz 192.168.3.204 14000 && nc -vz 192.168.3.204 14013 && nc -vz host.docker.internal 9090'
```

Bila gagal, izinkan lalu lintas dari subnet bridge Docker:

```sh
firewall-cmd --permanent --zone=trusted --add-interface=docker0
firewall-cmd --reload
```

Atau buka per port:

```sh
firewall-cmd --permanent --add-rich-rule='rule family=ipv4 source address=172.17.0.0/16 port port=9090 protocol=tcp accept'
firewall-cmd --permanent --add-rich-rule='rule family=ipv4 source address=172.17.0.0/16 port port=14000 protocol=tcp accept'
firewall-cmd --permanent --add-rich-rule='rule family=ipv4 source address=172.17.0.0/16 port port=14013 protocol=tcp accept'
firewall-cmd --reload
```

## 5. Daftarkan penerima dan jadwal

Volume SQLite baru berarti database kosong.

```sh
API=http://127.0.0.1:5118
K=<API_KEY>

curl -s -X POST "$API/api/recipients" \
  -H "X-API-Key: $K" -H 'Content-Type: application/json' \
  -d '{"name":"Falah","phone":"085161971784","active":true}'

curl -s -X PUT "$API/api/schedules" \
  -H "X-API-Key: $K" -H 'Content-Type: application/json' \
  -d '{"name":"Pagi","cron_expr":"34 7 * * *","active":true}'

curl -s -X PUT "$API/api/schedules" \
  -H "X-API-Key: $K" -H 'Content-Type: application/json' \
  -d '{"name":"Sore","cron_expr":"54 15 * * *","active":true}'
```

Cron di atas adalah **waktu pemicu**, bukan waktu kirim: siklus berjalan ~2,5
menit (batas keras 5 menit), jadi pemicunya dimajukan 6 menit supaya laporan
sudah sampai sebelum 07:40 dan 16:00. Kalau jadwal laporannya berbeda, geser
pemicunya sebesar margin yang sama.

Periksa hasilnya:

```sh
curl -s "$API/api/recipients" | python3 -m json.tool --indent 2
curl -s "$API/api/schedules" | python3 -m json.tool --indent 2
```

`PUT /api/schedules` memuat ulang cron di dalam container, jadi jadwal baru
langsung berlaku tanpa restart.

## 6. Sesi WhatsApp

Sesi Baileys tidak boleh dipakai dua instance sekaligus. Matikan stack di
laptop dulu (`docker compose down`), baru pindahkan atau buat sesi baru:

```sh
curl -s -H "X-API-Key: $K" "http://127.0.0.1:5118/api/qr" -o /tmp/qr.png
```

Buka `/tmp/qr.png` (atau `http://127.0.0.1:3101/qr` lewat SSH tunnel), lalu scan
dari ponsel yang memegang nomor gateway. Verifikasi:

```sh
curl -s "$API/api/wa/status" | python3 -m json.tool --indent 2
```

Bila sesi lama ingin dipindahkan, salin volume `baileys-session` dari laptop
sebelum kontainer di server dijalankan.

## 7. Sesi portal

Login mandiri berjalan di dalam siklus: password + kode TOTP dihitung lokal
(RFC 6238), tanpa browser dan tanpa layanan pihak ketiga. Timer token di laptop
harus dinonaktifkan supaya tidak saling tendang, karena portal hanya
mengizinkan satu sesi per akun:

```sh
systemctl --user disable --now scada-token-refresh.timer scada-token-watch.service
```

Periksa sesi portal:

```sh
curl -s "$API/api/session/status" | python3 -m json.tool --indent 2
```

## 8. Uji satu siklus penuh

`POST /api/report/run` benar-benar mengirim WhatsApp ke semua penerima aktif.

```sh
curl -s -X POST -H "X-API-Key: $K" "$API/api/report/run"
docker logs -f monitoring-api | grep -E 'siklus dimulai|sweep portal socket selesai|laporan tersimpan|pengiriman laporan'
docker logs monitoring-whatsapp --since 5m | grep -E 'status pesan|terkirim'
```

Pengumpulan telemetri butuh 60 detik bila semua RTU aktif dan sampai 10 menit
bila ada RTU yang diam. Pengiriman ditahan hanya bila pembacaan portal jatuh ke
cadangan REST (daftar tanpa status broker); RTU yang diam tetap masuk bagian
`Lambung no data from broker`.

## 9. Setelah pindah

- Hentikan stack di laptop (`docker compose down`) agar hanya ada satu pengirim.
- Nonaktifkan timer token portal di laptop (bagian 7).
- Riwayat laporan di volume laptop tetap tersimpan sebagai arsip.
- Backup rutin cukup untuk volume `sqlite-data` dan `baileys-session`.
