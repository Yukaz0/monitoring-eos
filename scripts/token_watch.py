#!/usr/bin/env python3
"""Pantau token portal SCADATR di profil Chromium dan dorong otomatis ke API.

Dijalankan terus-menerus sebagai systemd user service (scada-token-watch).
Setiap POLL_SECONDS:
  - ekstrak token terbaru dari LevelDB profil Chromium (extract_token.py;
    tidak butuh browser maupun CDP),
  - baca exp token yang sedang dipakai monitoring-api
    (GET /api/session/status -> token_exp),
  - dorong (PUT /api/session/token) HANYA bila token profil lebih baru.

Token portal lahir hanya dari login operator dan tidak punya endpoint refresh,
jadi begitu operator login di jendela portal profil otomasi, siklus laporan
berikutnya sudah memakai token baru tanpa perlu menjalankan skrip manual.
Karena hanya token yang lebih baru didorong, token yang masih sah tidak pernah
ditimpa token lama (mis. saat profil dibuka kembali dengan sesi kedaluwarsa).

Opsi:
    --once            periksa sekali lalu keluar (untuk uji manual)
    --force           dorong walau exp sama (uji jalur PUT)
    --interval N      detik antar pemeriksaan (default 20; env TOKEN_WATCH_INTERVAL)

Isi token TIDAK pernah dicetak. Exit code: 0 normal, 1 konfigurasi salah.
"""

import os
import sys
import time
import urllib.error
import urllib.request

from push_token import API_BASE, PROFILE, extract_token, load_api_key, push_token, token_expiry

DEFAULT_INTERVAL = 20
# Peringatkan lebih awal bila token yang ada tinggal sekian menit lagi.
WARN_MINUTES = 60
STATUS_URL = API_BASE.rstrip("/") + "/api/session/status"


def log(msg):
    print("%s %s" % (time.strftime("[%Y-%m-%d %H:%M:%S]"), msg), flush=True)


def api_token_exp():
    """exp token yang sedang dipakai monitoring-api, atau None bila tidak ada."""
    with urllib.request.urlopen(STATUS_URL, timeout=10) as resp:
        payload = resp.read().decode(errors="replace") or "{}"
    import json

    exp = (json.loads(payload) or {}).get("token_exp")
    return float(exp) if exp else None


def periksa(state, api_key, force=False):
    """Satu siklus pemeriksaan. state dipakai untuk menahan log berulang."""
    token = extract_token()
    if not token:
        if state.get("kabar") != "tanpa-token":
            log("belum ada token portal di profil %s (menunggu login operator)" % PROFILE)
            state["kabar"] = "tanpa-token"
        return
    exp, exp_str = token_expiry(token)
    if exp is None:
        log("PERINGATAN: exp token profil tidak terbaca; token tidak didorong")
        return

    sisa_menit = (exp - time.time()) / 60
    if sisa_menit <= 0:
        if state.get("kabar") != "kedaluwarsa" or state.get("exp") != exp:
            log("token profil sudah kedaluwarsa (%s); login ulang portal di profil otomatis" % exp_str)
            state["kabar"], state["exp"] = "kedaluwarsa", exp
        return

    try:
        api_exp = api_token_exp()
    except (urllib.error.URLError, OSError, ValueError) as exc:
        if state.get("kabar") != "api-mati":
            log("monitoring-api tidak bisa dihubungi: %s" % exc)
            state["kabar"] = "api-mati"
        return

    if api_exp is not None and api_exp >= exp and not force:
        if state.get("kabar") != "sudah-baru" or state.get("exp") != exp:
            log("token API sudah sama/lebih baru (%s); tidak mendorong" % exp_str)
            state["kabar"], state["exp"] = "sudah-baru", exp
        return

    try:
        status, body = push_token(token, api_key)
    except urllib.error.HTTPError as exc:
        log("GAGAL dorong token: HTTP %s %s" % (exc.code, exc.read().decode(errors="replace")[:120]))
        return
    except (urllib.error.URLError, OSError) as exc:
        log("GAGAL dorong token: monitoring-api tidak bisa dihubungi (%s)" % exc)
        return

    log("token baru didorong: berlaku sampai %s (HTTP %s %s)" % (exp_str, status, body))
    state["kabar"], state["exp"] = "didorong", exp
    if sisa_menit < WARN_MINUTES:
        log("PERINGATAN: token tinggal %.0f menit; login ulang sebelum jam laporan" % sisa_menit)


def parse_interval(argv):
    if "--interval" in argv:
        pos = argv.index("--interval")
        if pos + 1 >= len(argv):
            raise SystemExit("--interval butuh nilai detik")
        return max(5, int(argv[pos + 1]))
    return max(5, int(os.environ.get("TOKEN_WATCH_INTERVAL", DEFAULT_INTERVAL)))


def main(argv):
    once = "--once" in argv
    force = "--force" in argv
    interval = parse_interval(argv)

    api_key = load_api_key()
    if not api_key:
        log("GAGAL: API_KEY tidak ditemukan (env atau .env repo)")
        return 1

    state = {}
    if not once:
        log("pemantau token jalan: periksa tiap %d detik (profil %s)" % (interval, PROFILE))
    while True:
        try:
            periksa(state, api_key, force=force)
        except KeyboardInterrupt:
            return 0
        except Exception as exc:  # jangan mati karena error tak terduga
            log("ERROR tak terduga: %s: %s" % (type(exc).__name__, exc))
        if once:
            return 0
        # force hanya untuk satu kali dorong pada mode sekali-jalan.
        force = False
        time.sleep(interval)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
