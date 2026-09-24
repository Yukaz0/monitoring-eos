#!/usr/bin/env python3
"""Verifikasi login REST portal + baca balik lewat token (read-only).

Membuktikan rantai yang dipakai siklus: kredensial -> /users/login (opsional
/users/verify-2fa) -> token -> REST master-rtu + event socket.io "mqtt".

Tidak mengirim laporan, tidak mengubah apa pun di portal, dan tidak pernah
mencetak token/password. Kredensial dibaca dari .env repo.

Contoh:
    python3 scripts/cek_login_portal.py
    python3 scripts/cek_login_portal.py --jendela 15
"""
from __future__ import annotations

import argparse
import base64
import hashlib
import hmac
import json
import os
import ssl
import struct
import sys
import time
import urllib.error
import urllib.request
from urllib.parse import urlparse

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DEFAULT_ENV = os.path.join(REPO, ".env")


def baca_env(path: str) -> dict[str, str]:
    out: dict[str, str] = {}
    if not os.path.exists(path):
        return out
    with open(path, encoding="utf-8") as fh:
        for line in fh:
            line = line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            k, _, v = line.partition("=")
            v = v.strip()
            if len(v) >= 2 and v[0] == v[-1] and v[0] in "\"'":
                v = v[1:-1]
            out[k.strip()] = v
    return out


CTX = ssl.create_default_context()
CTX.check_hostname = False
CTX.verify_mode = ssl.CERT_NONE


def req(method, url, body=None, bearer=None, timeout=30, ctype="text/plain;charset=UTF-8"):
    data = body.encode() if isinstance(body, str) else body
    r = urllib.request.Request(url, data=data, method=method)
    r.add_header("Content-Type", ctype)
    if bearer:
        r.add_header("Authorization", "Bearer " + bearer)
    try:
        with urllib.request.urlopen(r, context=CTX, timeout=timeout) as resp:
            return resp.status, resp.read()
    except urllib.error.HTTPError as e:
        return e.code, e.read()


def jwt_exp(token: str):
    """Baca klaim exp tanpa verifikasi tanda tangan."""
    try:
        part = token.split(".")[1]
        part += "=" * (-len(part) % 4)
        payload = json.loads(base64.urlsafe_b64decode(part))
        exp = float(payload["exp"])
        return exp, time.strftime("%Y-%m-%d %H:%M:%S %Z", time.localtime(exp))
    except Exception:  # noqa: BLE001
        return None, None


def cari_token(obj):
    """Cari nilai token di respons login (toleran, termasuk auth string JSON)."""
    if isinstance(obj, dict):
        for k in ("token", "access_token"):
            v = obj.get(k)
            if isinstance(v, str) and v:
                return v
        for k in ("data", "auth", "userData"):
            if k in obj:
                t = cari_token(obj[k])
                if t:
                    return t
        for v in obj.values():
            t = cari_token(v)
            if t:
                return t
    elif isinstance(obj, str):
        s = obj.strip()
        if s.startswith("{"):
            try:
                return cari_token(json.loads(s))
            except ValueError:
                return ""
    return ""


def totp(secret: str, t: float) -> str:
    norm = secret.replace(" ", "").replace("-", "").upper()
    norm += "=" * (-len(norm) % 8)
    key = base64.b32decode(norm)
    counter = int(t // 30)
    mac = hmac.new(key, struct.pack(">Q", counter), hashlib.sha1).digest()
    off = mac[-1] & 0x0F
    code = struct.unpack(">I", mac[off:off + 4])[0] & 0x7FFFFFFF
    return "%06d" % (code % 1000000)


def simpan_ke_env(path: str, key: str, value: str) -> None:
    """Tulis/ubah satu kunci di .env tanpa mencetak nilainya.

    Nilai hanya masuk ke file; stdout tetap bersih supaya rahasia tidak pernah
    ikut ke log percakapan.
    """
    baris: list[str] = []
    ada = False
    if os.path.exists(path):
        with open(path, encoding="utf-8") as fh:
            baris = fh.read().splitlines()
    for i, b in enumerate(baris):
        if b.startswith(key + "="):
            baris[i] = f"{key}={value}"
            ada = True
            break
    if not ada:
        baris.append(f"{key}={value}")
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as fh:
        fh.write("\n".join(baris) + "\n")
    os.replace(tmp, path)
    os.chmod(path, 0o600)


def simpan_qr(data_url: str, path: str) -> bool:
    """Simpan gambar QR enrollment (data URL base64) ke file. false bila kosong.

    QR dipakai agar operator bisa memindai secret yang sama ke aplikasi
    authenticator di HP; secret-nya tidak perlu dikirim ke layanan mana pun.
    """
    if not data_url.startswith("data:") or "," not in data_url:
        return False
    try:
        b64 = data_url.split(",", 1)[1]
        with open(path, "wb") as fh:
            fh.write(base64.b64decode(b64))
    except (OSError, ValueError):
        return False
    os.chmod(path, 0o600)
    return True


def main() -> int:
    ap = argparse.ArgumentParser(description="Verifikasi login REST portal (read-only)")
    ap.add_argument("--env", default=DEFAULT_ENV)
    ap.add_argument("--jendela", type=float, default=10.0, help="lama tunggu event mqtt (detik)")
    ap.add_argument("--simpan-totp", action="store_true",
                    help="bila portal meminta enrollment, simpan manual_setup ke .env sebagai PORTAL_TOTP_SECRET")
    ap.add_argument("--kode", action="store_true",
                    help="cetak kode TOTP saat ini dari PORTAL_TOTP_SECRET (padanan lokal 2fa.live)")
    ap.add_argument("--simpan-qr", default="",
                    help="saat enrollment, simpan gambar QR ke path ini agar bisa dipindai ke aplikasi authenticator")
    args = ap.parse_args()

    env = baca_env(args.env)
    user = env.get("PORTAL_USERNAME", "")
    pw = env.get("PORTAL_PASSWORD", "")
    totp_secret = env.get("PORTAL_TOTP_SECRET", "")

    if args.kode:
        # Padanan lokal 2fa.live: kode dihitung di mesin ini, secret tidak
        # pernah dikirim ke layanan mana pun.
        if not totp_secret:
            print("PORTAL_TOTP_SECRET kosong di .env — belum ada yang bisa dihitung.")
            return 2
        sekarang = time.time()
        sisa = 30 - int(sekarang) % 30
        print(f"kode TOTP sekarang: {totp(totp_secret, sekarang)} (berlaku {sisa} detik lagi)")
        return 0

    if not user or not pw:
        print("PORTAL_USERNAME / PORTAL_PASSWORD belum diisi di .env — stop.")
        return 2

    host = env.get("PORTAL_HOST", "scadatr.grita.id")
    sysport = int(env.get("PORTAL_SYSTEM_PORT", "14000") or 14000)
    mqttport = int(env.get("PORTAL_MQTT_PORT", "14013") or 14013)
    base = f"https://{host}:{sysport}"
    print(f"portal   : {base}")
    print(f"akun     : {user} (password terisi)")

    st, body = req("POST", base + "/users/login",
                   json.dumps({"username": user, "password": pw}), ctype="application/json")
    print(f"login    : HTTP {st}")
    if st != 200:
        print(f"  respons: {body[:200]!r}")
        return 1
    try:
        obj = json.loads(body)
    except ValueError:
        print(f"  respons bukan JSON: {body[:200]!r}")
        return 1

    data = obj.get("data") if isinstance(obj.get("data"), dict) else {}

    def ambil(k):
        v = obj.get(k)
        return v if v not in (None, "") else data.get(k)

    dua_faktor = bool(ambil("two_factor_required"))
    token = cari_token(obj)
    if not token and dua_faktor:
        pre = ambil("pre_auth_token") or ""
        ttl = ambil("pre_auth_expires_in") or 300
        perlu_qr = bool(ambil("requires_qr_setup"))
        print(f"  2FA diminta (pre_auth_token berlaku {ttl} detik, requires_qr_setup={perlu_qr})")
        if perlu_qr:
            # Akun belum mendaftarkan authenticator: portal mengirim secret
            # manual (base32). Disimpan langsung ke .env, tidak pernah dicetak.
            manual = ambil("manual_setup") or ""
            if not manual:
                print(f"  requires_qr_setup tapi manual_setup kosong (qr_code ada: {bool(ambil('qr_code'))})")
                return 1
            if not args.simpan_totp:
                print(f"  manual_setup tersedia (panjang {len(manual)}) — jalankan ulang dengan "
                      "--simpan-totp untuk menyimpannya ke .env, lalu saya lanjutkan verifikasi.")
                return 1
            simpan_ke_env(args.env, "PORTAL_TOTP_SECRET", manual)
            print(f"  manual_setup disimpan ke {args.env} sebagai PORTAL_TOTP_SECRET (panjang {len(manual)})")
            totp_secret = manual
            if args.simpan_qr:
                qr = ambil("qr_code") or ""
                if simpan_qr(qr, args.simpan_qr):
                    print(f"  QR enrollment disimpan ke {args.simpan_qr} — pindai ke aplikasi "
                          "authenticator kalau ingin kode di HP juga (secret sama, keduanya sah)")
                else:
                    print("  qr_code tidak tersedia/tidak terbaca; pakai kode dari .env saja")
        if not totp_secret:
            print("  Akun sudah terdaftar authenticator, jadi portal minta kode dari secret yang ADA.")
            print("  Pilihan: (a) isi PORTAL_TOTP_SECRET dari aplikasi authenticator operator,")
            print("           (b) minta DBA reset/daftar ulang MFA akun otomasi lalu jalankan --simpan-totp,")
            print("           (c) minta DBA set mfa_exempt=true untuk akun otomasi (tanpa 2FA).")
            return 1
        for geser in (0, -30, 30):
            kode = totp(totp_secret, time.time() + geser)
            st2, body2 = req("POST", base + "/users/verify-2fa",
                             json.dumps({"pre_auth_token": pre, "totp_code": kode}),
                             ctype="application/json")
            print(f"  verify-2fa (geser {geser:+d}s): HTTP {st2}")
            if st2 == 200:
                token = cari_token(json.loads(body2))
                if token:
                    break
    if not token:
        print("  token tidak ditemukan di respons -> login gagal")
        return 1

    exp, exp_str = jwt_exp(token)
    sisa = (exp - time.time()) / 3600 if exp else None
    print(f"token    : diterima (panjang {len(token)}), exp {exp_str}"
          + (f", sisa {sisa:.2f} jam" if sisa is not None else ""))

    # 1. Bukti token dipakai: REST master-rtu.
    st, body = req("POST", base + "/master-rtu/dropdown-rtu-by-protocol",
                   json.dumps({"protocol": "MQTT"}), bearer=token, ctype="application/json")
    jumlah = "-"
    if st == 200:
        try:
            jumlah = len(json.loads(body).get("data") or [])
        except ValueError:
            pass
    print(f"master-rtu: HTTP {st}, entri RTU = {jumlah}")

    # 2. Event socket.io "mqtt": daftar klien + status broker + host/port.
    sbase = f"https://{host}:{mqttport}/socket.io/?EIO=4&transport=polling"
    st, body = req("GET", sbase, bearer=token)
    if st != 200 or not body or body[0:1] != b"0":
        print(f"socket.io: handshake HTTP {st} gagal -> {body[:120]!r}")
        return 1
    sid = json.loads(body[1:].decode())["sid"]
    surl = sbase + "&sid=" + sid
    req("POST", surl, "40", bearer=token)
    req("POST", surl, '42["data-mqtt"]', bearer=token)

    klien, ditolak = [], False
    deadline = time.time() + args.jendela
    while time.time() < deadline and not klien:
        try:
            st, body = req("GET", surl, bearer=token, timeout=max(1.0, deadline - time.time()))
        except Exception as e:  # noqa: BLE001
            print(f"socket.io: poll berhenti ({type(e).__name__})")
            break
        for msg in body.split(b"\x1e"):
            msg = msg.strip()
            if not msg:
                continue
            if msg[0:1] == b"2":
                req("POST", surl, "3", bearer=token)
                continue
            if msg[0:2] != b"42":
                continue
            try:
                arr = json.loads(msg[2:].decode())
            except ValueError:
                continue
            if not isinstance(arr, list) or not arr:
                continue
            if arr[0] == "un-authorized":
                ditolak = True
            if arr[0] == "mqtt" and isinstance(arr[1], list):
                klien = arr[1]

    if ditolak:
        print("socket.io: portal menolak token (event un-authorized)")
        return 1
    print(f"socket.io: {len(klien)} klien pada event 'mqtt'")
    if klien:
        def aktif(c):
            return str(c.get("status_mqtt")).lower() in ("true", "1")
        print(f"  status_mqtt true: {sum(1 for c in klien if aktif(c))} / {len(klien)}")
        host_count = {}
        for c in klien:
            kunci = f"{c.get('host')}:{c.get('port')}"
            host_count[kunci] = host_count.get(kunci, 0) + 1
        print("  broker per klien:")
        for h, n in sorted(host_count.items(), key=lambda kv: -kv[1]):
            print(f"    {n:3d} klien  {h}")
        print("  contoh 8 klien (id_rtu | name | station | host:port | status):")
        for c in klien[:8]:
            stasiun = c.get("station")
            if isinstance(stasiun, dict):
                stasiun = stasiun.get("name_station")
            print(f"    {c.get('id_rtu')} | {c.get('name')} | {stasiun} | "
                  f"{c.get('host')}:{c.get('port')} | {c.get('status_mqtt')}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
