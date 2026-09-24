#!/usr/bin/env python3
"""Probe read-only broker MQTT SCADATR (EMQX).

Membaca kredensial dari .env (MQTT_URL/MQTT_USERNAME/MQTT_PASSWORD/MQTT_TOPICS),
menyambung, berlangganan, lalu melaporkan ringkasan traffic. Kredensial tidak
pernah dicetak.

Contoh:
    python3 scripts/probe_mqtt.py --window 60
    python3 scripts/probe_mqtt.py --topics '$SYS/#' --window 30
    python3 scripts/probe_mqtt.py --verbose        # tampilkan contoh payload
"""
from __future__ import annotations

import argparse
import os
import re
import socket
import ssl
import struct
import sys
import time
from collections import Counter, defaultdict
from urllib.parse import urlparse

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
DEFAULT_ENV = os.path.join(REPO, ".env")

# Topik lambung laporan berbentuk UPS_01_FUY/#, PV_KC_BRI_GUNUNG_SAHARI/#, K3E/#
LAMBUNG_RE = re.compile(r"^[A-Z0-9]+_[0-9A-Z_]+")


def baca_env(path: str) -> dict[str, str]:
    out: dict[str, str] = {}
    if not os.path.exists(path):
        return out
    with open(path, encoding="utf-8") as fh:
        for line in fh:
            line = line.strip()
            if not line or line.startswith("#") or "=" not in line:
                continue
            key, _, value = line.partition("=")
            key = key.strip()
            value = value.strip()
            if len(value) >= 2 and value[0] == value[-1] and value[0] in "\"'":
                value = value[1:-1]
            out[key] = value
    return out


# --- MQTT 3.1.1 minimal (tanpa dependensi eksternal) -----------------------


def enc_len(n: int) -> bytes:
    out = b""
    while True:
        b = n % 128
        n //= 128
        out += bytes([b | 0x80]) if n else bytes([b])
        if not n:
            return out


def enc_str(s: str) -> bytes:
    b = s.encode()
    return struct.pack("!H", len(b)) + b


def packet(header: int, body: bytes) -> bytes:
    return bytes([header]) + enc_len(len(body)) + body


class Klien:
    def __init__(self, url: str, username: str, password: str, client_id: str, keepalive: int = 60):
        u = urlparse(url if "://" in url else "tcp://" + url)
        self.host = u.hostname or "127.0.0.1"
        self.port = u.port or 1883
        self.tls = u.scheme in ("ssl", "tls", "mqtts", "wss")
        self.username = username
        self.password = password
        self.client_id = client_id
        self.keepalive = keepalive
        self.sock: socket.socket | None = None
        self._next_id = 1

    def sambung(self) -> int:
        raw = socket.create_connection((self.host, self.port), timeout=10)
        if self.tls:
            ctx = ssl.create_default_context()
            raw = ctx.wrap_socket(raw, server_hostname=self.host)
        self.sock = raw
        flags = 0x02  # clean session
        if self.username:
            flags |= 0x80
            if self.password:
                flags |= 0x40
        vh = enc_str("MQTT") + bytes([4, flags]) + struct.pack("!H", self.keepalive)
        payload = enc_str(self.client_id)
        if self.username:
            payload += enc_str(self.username)
            if self.password:
                payload += enc_str(self.password)
        self._kirim(packet(0x10, vh + payload))
        hdr, body = self._baca()
        if hdr != 0x20 or len(body) < 2:
            raise RuntimeError(f"CONNACK tidak dikenal: hdr={hdr!r} body={body!r}")
        return body[1]

    def langganan(self, topic: str, qos: int = 0, topik: Counter | None = None,
                  contoh: dict[str, bytes] | None = None, verbose: bool = False) -> int:
        pid = self._next_id
        self._next_id += 1
        self._kirim(packet(0x82, struct.pack("!H", pid) + enc_str(topic) + bytes([qos])))
        # Broker boleh mengirim PUBLISH (retained) sebelum SUBACK tiba: paket
        # itu harus diproses sebagai traffic, bukan disalahartikan sebagai
        # SUBACK. Tanpa ini, satu retained message menggagalkan seluruh probe.
        while True:
            hdr, body = self._baca()
            if hdr is None:
                raise RuntimeError("koneksi ditutup broker sebelum SUBACK")
            jenis = hdr >> 4
            if jenis == 9:  # SUBACK
                return body[-1] if body else 0xFF
            if jenis == 3:  # PUBLISH
                self._tangani_publish(hdr, body, topik, contoh, verbose)
                continue
            # PINGRESP dan paket kontrol lain diabaikan.

    def ping(self) -> None:
        self._kirim(packet(0xC0, b""))

    def tutup(self) -> None:
        if self.sock:
            try:
                self._kirim(packet(0xE0, b""))
            except OSError:
                pass
            self.sock.close()
            self.sock = None

    def _kirim(self, data: bytes) -> None:
        assert self.sock is not None
        self.sock.sendall(data)

    def _baca(self) -> tuple[int | None, bytes]:
        assert self.sock is not None
        hdr = self.sock.recv(1)
        if not hdr:
            return None, b""
        mult, length = 1, 0
        while True:
            b = self.sock.recv(1)
            if not b:
                return None, b""
            length += (b[0] & 127) * mult
            if not b[0] & 0x80:
                break
            mult *= 128
        body = b""
        while len(body) < length:
            chunk = self.sock.recv(length - len(body))
            if not chunk:
                break
            body += chunk
        return hdr[0], body

    def _tangani_publish(self, hdr: int, body: bytes, topik: Counter | None,
                         contoh: dict[str, bytes] | None, verbose: bool) -> None:
        """Catat satu PUBLISH dari broker: topik, contoh payload, dan ack QoS 1."""
        qos = (hdr >> 1) & 3
        tlen = struct.unpack("!H", body[:2])[0]
        topic = body[2:2 + tlen].decode(errors="replace")
        sisa = body[2 + tlen:]
        if qos > 0 and len(sisa) >= 2:
            pid = struct.unpack("!H", sisa[:2])[0]
            if qos == 1:
                self._kirim(packet(0x40, struct.pack("!H", pid)))
            sisa = sisa[2:]
        if topik is not None:
            topik[topic] += 1
        if contoh is not None and topic not in contoh:
            contoh[topic] = sisa
        if verbose:
            print(f"    {topic} -> {sisa[:120]!r}")

    def putaran(self, batas: float, verbose: bool, topik: Counter, contoh: dict[str, bytes]) -> int:
        """Baca paket sampai `batas`; kembalikan jumlah PUBLISH yang diterima."""
        assert self.sock is not None
        publish = 0
        while time.time() < batas:
            self.sock.settimeout(max(0.5, min(5.0, batas - time.time())))
            try:
                hdr, body = self._baca()
            except socket.timeout:
                self.ping()
                continue
            except OSError as e:
                print(f"  koneksi berhenti: {type(e).__name__}: {e}")
                break
            if hdr is None:
                print("  broker menutup koneksi")
                break
            jenis = hdr >> 4
            if jenis == 3:  # PUBLISH
                self._tangani_publish(hdr, body, topik, contoh, verbose)
                publish += 1
            elif jenis == 13:  # PINGRESP
                pass
        return publish


def main() -> int:
    ap = argparse.ArgumentParser(description="Probe read-only broker MQTT SCADATR")
    ap.add_argument("--env", default=DEFAULT_ENV, help="path file .env (default: repo/.env)")
    ap.add_argument("--url", default=None, help="override MQTT_URL")
    ap.add_argument("--topics", default=None, help="override MQTT_TOPICS (pisah koma)")
    ap.add_argument("--window", type=float, default=60.0, help="lama pengumpulan detik (default 60)")
    ap.add_argument("--verbose", action="store_true", help="cetak contoh payload")
    args = ap.parse_args()

    env = baca_env(args.env)
    url = args.url or env.get("MQTT_URL", "")
    username = env.get("MQTT_USERNAME", "")
    password = env.get("MQTT_PASSWORD", "")
    topik_cfg = args.topics or env.get("MQTT_TOPICS", "#")
    if not url:
        print(f"MQTT_URL belum ada di {args.env}. Isi dulu: MQTT_URL, MQTT_USERNAME, MQTT_PASSWORD.")
        return 2

    topik_list = [t.strip() for t in topik_cfg.split(",") if t.strip()]
    print(f"broker   : {url}")
    print(f"akun     : {'(terisi)' if username else '(kosong)'} / password {'(terisi)' if password else '(kosong)'}")
    print(f"topik    : {topik_list}")
    print(f"jendela  : {args.window:.0f} detik")

    k = Klien(url, username, password, "monitoring-eos-probe")
    try:
        rc = k.sambung()
    except Exception as e:  # noqa: BLE001
        print(f"CONNECT gagal: {type(e).__name__}: {e}")
        return 1
    nama_rc = {0: "DITERIMA", 1: "versi protokol ditolak", 2: "client id ditolak",
               3: "server tidak tersedia", 4: "username/password salah", 5: "TIDAK DIIZINKAN"}
    print(f"CONNACK  : {rc} -> {nama_rc.get(rc, 'kode lain')}")
    if rc != 0:
        k.tutup()
        return 1

    topik: Counter = Counter()
    contoh: dict[str, bytes] = {}
    for t in topik_list:
        try:
            # Retained message bisa datang sebelum SUBACK dan langsung dicatat
            # ke `topik` — jadi tidak ada traffic yang hilang.
            granted = k.langganan(t, 0, topik, contoh, args.verbose)
        except Exception as e:  # noqa: BLE001
            print(f"SUBSCRIBE {t} gagal: {type(e).__name__}: {e}")
            k.tutup()
            return 1
        print(f"SUBACK   : {t} -> {granted if granted != 0x80 else 'DITOLAK (0x80, cek ACL)'}")

    try:
        k.putaran(time.time() + args.window, args.verbose, topik, contoh)
    except KeyboardInterrupt:
        pass
    finally:
        k.tutup()
    total = sum(topik.values())

    print(f"\ntraffic  : {total} pesan dari {len(topik)} topik")
    print("\n20 topik tersibuk:")
    for t, n in topik.most_common(20):
        print(f"  {n:6d}  {t}")
    lambung = sorted(t for t in topik if LAMBUNG_RE.match(t))
    print(f"\ntopik bergaya lambung ({len(lambung)}):")
    for t in lambung[:30]:
        print(f"  {topik[t]:6d}  {t}")
    sys_topik = sorted(t for t in topik if t.startswith("$SYS/"))
    print(f"\ntopik $SYS ({len(sys_topik)}):")
    for t in sys_topik[:15]:
        print(f"  {topik[t]:6d}  {t}")
    if contoh:
        print("\ncontoh payload (terpotong 120 byte):")
        for t in sorted(contoh)[:10]:
            print(f"  {t} -> {contoh[t][:120]!r}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
