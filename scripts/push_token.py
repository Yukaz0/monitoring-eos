#!/usr/bin/env python3
"""Kirim token portal terbaru dari profil Chromium operator ke monitoring-api.

Token JWT portal SCADATR berumur sekitar 24 jam, sedangkan siklus laporan di
container mengambil token dari SQLite (diisi lewat PUT /api/session/token).
Skrip ini dipakai systemd user timer sebelum jam laporan supaya sesi portal
tidak mati saat sweep dijalankan.

Exit code: 0 sukses, 1 gagal (token tidak ditemukan / API menolak).
Opsi: --force mendorong walau token di API sudah sama/lebih baru.
Isi token TIDAK pernah dicetak.
"""

import json
import os
import subprocess
import sys
import urllib.error
import urllib.request

API_BASE = os.environ.get("MONITORING_API_URL", "http://localhost:5118")
PROFILE = os.environ.get("SCADA_CHROMIUM_PROFILE", os.path.expanduser("~/.cache/scada-chromium-1"))
HERE = os.path.dirname(os.path.abspath(__file__))
EXTRACT = os.path.join(HERE, "extract_token.py")
ENV_FILE = os.path.abspath(os.path.join(HERE, "..", ".env"))


def load_api_key():
    key = os.environ.get("API_KEY", "").strip()
    if key:
        return key
    try:
        with open(ENV_FILE) as fh:
            for line in fh:
                if line.startswith("API_KEY="):
                    return line.split("=", 1)[1].strip()
    except OSError:
        pass
    return ""


def extract_token():
    res = subprocess.run([sys.executable, EXTRACT, "--profile", PROFILE],
                         capture_output=True, text=True)
    if res.returncode != 0:
        print("GAGAL ekstrak token:", (res.stderr or "").strip()[:200])
        return None
    token = res.stdout.strip()
    if not token:
        print("GAGAL: extract_token.py tidak mengeluarkan token")
        return None
    return token


def push_token(token, api_key):
    body = json.dumps({"token": token}).encode()
    req = urllib.request.Request(API_BASE.rstrip("/") + "/api/session/token",
                                 data=body, method="PUT",
                                 headers={"X-API-Key": api_key, "Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=20) as resp:
        return resp.status, resp.read().decode(errors="replace")[:120]


def session_status():
    try:
        with urllib.request.urlopen(API_BASE.rstrip("/") + "/api/session/status", timeout=10) as resp:
            return resp.read().decode(errors="replace")[:120]
    except OSError as exc:
        return "tidak bisa dibaca: %s" % exc


def api_token_exp():
    """exp token yang sedang dipakai monitoring-api, atau None bila tidak terbaca."""
    try:
        with urllib.request.urlopen(API_BASE.rstrip("/") + "/api/session/status", timeout=10) as resp:
            data = json.loads(resp.read().decode(errors="replace") or "{}")
    except (OSError, ValueError):
        return None
    exp = data.get("token_exp")
    return float(exp) if exp else None


def token_expiry(token):
    """Baca exp token tanpa verifikasi tanda tangan. Kembalikan (epoch, teks) atau (None, None).

    Menerima dua bentuk: JWT mentah, atau isi localStorage `persist:grita-scada-tr`
    (JSON dengan auth -> token / userData.token) seperti yang dikeluarkan
    scripts/extract_token.py.
    """
    import base64
    import time

    jwt = token.strip()
    if jwt.startswith("{"):
        try:
            obj = json.loads(jwt)
            auth = obj.get("auth")
            auth = json.loads(auth) if isinstance(auth, str) else auth
            user_data = (auth or {}).get("userData") or {}
            jwt = user_data.get("token") or (auth or {}).get("token") or ""
        except (ValueError, AttributeError):
            return None, None
    try:
        part = jwt.split(".")[1]
        part += "=" * (-len(part) % 4)
        payload = json.loads(base64.urlsafe_b64decode(part))
        exp = float(payload["exp"])
        return exp, time.strftime("%Y-%m-%d %H:%M", time.localtime(exp))
    except (IndexError, KeyError, ValueError):
        return None, None


def main(argv=None):
    argv = list(sys.argv[1:] if argv is None else argv)
    force = "--force" in argv
    api_key = load_api_key()
    if not api_key:
        print("GAGAL: API_KEY tidak ditemukan (env atau .env repo)")
        return 1
    token = extract_token()
    if not token:
        return 1

    exp, exp_str = token_expiry(token)
    if exp is None:
        print("PERINGATAN: exp token tidak bisa dibaca; melanjutkan tanpa validasi")
    else:
        import time
        sisa_jam = (exp - time.time()) / 3600
        print("token berlaku sampai %s (sisa %.2f jam)" % (exp_str, sisa_jam))
        # Token portal lahir dari login operator dan tidak punya endpoint refresh:
        # bila sudah/mendekati kedaluwarsa, laporan terjadwal akan kehilangan
        # bagian telemetri, jadi kegagalan harus terlihat di log.
        if sisa_jam <= 0:
            print("GAGAL: token sudah kedaluwarsa. Login ulang portal di profil "
                  "%s lalu jalankan skrip ini lagi." % PROFILE)
            return 2
        if sisa_jam < 1:
            print("PERINGATAN: token kedaluwarsa kurang dari 1 jam lagi; "
                  "login ulang portal di profil otomasi sebelum jam laporan.")

    # Jangan menimpa token yang masih sah dengan token lama: profil bisa saja
    # berisi sesi kedaluwarsa sementara API sudah memakai token yang lebih baru
    # (mis. hasil login berikutnya). --force melewati pemeriksaan ini.
    if exp is not None and not force:
        api_exp = api_token_exp()
        if api_exp is not None and api_exp >= exp:
            print("token di API sudah sama/lebih baru; tidak mendorong "
                  "(pakai --force untuk memaksa).")
            return 0

    try:
        status, body = push_token(token, api_key)
    except urllib.error.HTTPError as exc:
        print("GAGAL: PUT /api/session/token -> HTTP %s %s" % (exc.code, exc.read().decode(errors="replace")[:120]))
        return 1
    except OSError as exc:
        print("GAGAL: monitoring-api tidak bisa dihubungi: %s" % exc)
        return 1
    print("token dikirim (panjang %d), HTTP %s %s" % (len(token), status, body))
    print("status sesi:", session_status())
    return 0


if __name__ == "__main__":
    sys.exit(main())
