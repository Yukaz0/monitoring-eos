#!/usr/bin/env python3
"""Opsi A: jalankan laporan harian SCADATR dari HOST dan kirim ke container API.

Reuse logika skill scada-daily-reporting (scripts/scada_check.py): launcher
headful Chromium pada profil ~/.cache/scada-chromium-1 via CDP 9223,
model auth via localStorage persist:grita-scada-tr, sweep RTU per anchor
klik, Prometheus metrics. Output dirender sesuai template resmi, lalu
dikirim ke container API POST /api/report/submit.

Exit code: 0 sukses; 2 sesi portal mati; 1 kesalahan lain.
"""
import base64
import datetime
import json
import os
import subprocess
import sys
import time
import urllib.parse
import urllib.request

# Reuse fungsi dari skill scada_check.py tanpa memodifikasinya.
sys.path.insert(0, "/home/neu/.hermes/skills/operations/scada-daily-reporting/scripts")
import scada_check  # noqa: E402
import websocket  # noqa: E402

API_BASE = os.environ.get("MONITORING_API_URL", "http://localhost:5118")
API_KEY = os.environ.get("API_KEY", "")
MQTT_URL = scada_check.MQTT_URL
PROM_URL = scada_check.PROM


def api_post(path, body):
    data = json.dumps(body).encode()
    req = urllib.request.Request(
        base_mangle(path), data=data, method="POST",
        headers={"X-API-Key": API_KEY, "Content-Type": "application/json"})
    # Server menyimpan laporan lebih dulu, lalu mengirim WhatsApp. Pengiriman
    # bisa makan waktu puluhan detik, jadi beri ruang 180 detik agar skrip
    # tidak crash setelah laporan tersimpan.
    with urllib.request.urlopen(req, timeout=180) as r:
        return r.status, json.loads(r.read())


def base_mangle(path):
    return API_BASE.rstrip("/") + path


def jwt_exp_days_left(jwt):
    p = jwt.split(".")[1]
    p += "=" * (-len(p) % 4)
    c = json.loads(base64.urlsafe_b64decode(p))
    return (c["exp"] - time.time()) / 86400


def load_api_key():
    key = os.environ.get("API_KEY", "")
    if key:
        return key
    env_path = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", ".env")
    env_path = os.path.abspath(env_path)
    if os.path.exists(env_path):
        for line in open(env_path):
            line = line.strip()
            if line.startswith("API_KEY="):
                return line.split("=", 1)[1].strip()
    return ""


def main():
    global API_KEY
    API_KEY = load_api_key()
    if not API_KEY:
        print("GAGAL: API_KEY tidak ditemukan (env atau .env repo).")
        return 1

    # Full sweep from the host (Chromium headful + CDP 9223)
    rc = scada_check.main()
    if rc != 0:
        print("GAGAL: sweep portal dari host tidak sukses (rc=%d)." % rc)
        return rc if rc == 2 else 1

    # Bila sukses, scada_check.main sudah menulis laporan resmi di
    # ~/Downloads; kirim isi laporan terakhir ke API.
    now = datetime.datetime.now()
    out = os.path.expanduser("~/Downloads/Laporan Harian SCADATR UP2D JAYA - %s.txt"
                             % now.strftime("%Y-%m-%d"))
    if not os.path.exists(out):
        print("GAGAL: file laporan %s tidak ditemukan." % out)
        return 1
    with open(out) as fh:
        report = fh.read().rstrip("\n")

    status, resp = api_post("/api/report/submit", {"content": report})
    print("submit status:", status, "report_id:", resp.get("id") if isinstance(resp, dict) else resp)
    return 0


if __name__ == "__main__":
    sys.exit(main())
