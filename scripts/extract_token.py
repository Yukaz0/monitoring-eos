#!/usr/bin/env python3
"""Ekstrak token portal SCADATR (persist:grita-scada-tr) dari LevelDB
Local Storage profil Chromium.

Dipakai di laptop operator (bukan di container). Membaca file
Default/Local Storage/leveldb/{000003.log,*.ldb} dari profil yang pernah
login portal, mencari key persist:grita-scada-tr, lalu mengambil nilai
JSON mentah (raw bytes diawali '{' setelah occurrence terakhir key,
diakhiri balanced brace), memvalidasi json.loads, dan mencetaknya ke
stdout. Isi token TIDAK dicetak ke log.

Contoh:
    python3 scripts/extract_token.py --profile ~/.cache/scada-chromium-1
"""

import argparse
import glob
import json
import os
import sys


def read_key_prefix_bytes(key: bytes) -> list[bytes]:
    """Bentuk varian byte key Local Storage: prefix '_<origin>\\x00\\x01<key>'."""
    origin = b"https://scadatr.grita.id"
    # LevelDB Local Storage menyimpan key sebagai: b'_' + origin + b'\x00\x01' + key
    return [b"_" + origin + b"\x00\x01" + key]


def find_last_occurrence(data: bytes, needle: bytes) -> int:
    """Index occurrence terakhir needle dalam data, atau -1."""
    return data.rfind(needle)


def extract_balanced_json(data: bytes, start: int) -> bytes | None:
    """Ambil raw bytes JSON diawali '{' mulai posisi start sampai brace seimbang.

    Bila brace tidak seimbang (truncation), kembalikan None.
    """
    depth = 0
    in_str = False
    esc = False
    for i in range(start, len(data)):
        b = data[i]
        if in_str:
            if esc:
                esc = False
            elif b == 0x5C:  # backslash
                esc = True
            elif b == 0x22:  # double quote
                in_str = False
            continue
        if b == 0x22:
            in_str = True
        elif b == 0x7B:  # '{'
            depth += 1
        elif b == 0x7D:  # '}'
            depth -= 1
            if depth == 0:
                return data[start : i + 1]
    return None


def scan_file(path: str, key_variants: list[bytes]) -> list[bytes]:
    """Scan satu file LevelDB (.log atau .ldb) untuk nilai JSON yang valid."""
    found: list[bytes] = []
    with open(path, "rb") as f:
        data = f.read()
    for key in key_variants:
        pos = find_last_occurrence(data, key)
        while pos != -1:
            value_start = pos + len(key)
            # Lewati byte non-'{' sesudah key (mis. marker panjang LevelDB).
            while value_start < len(data) and data[value_start] != 0x7B:
                value_start += 1
                if value_start - pos > 64:
                    break
            if value_start >= len(data) or data[value_start] != 0x7B:
                pos = find_last_occurrence(data[:pos], key)
                continue
            raw = extract_balanced_json(data, value_start)
            if raw is None:
                pos = find_last_occurrence(data[:pos], key)
                continue
            try:
                json.loads(raw.decode("utf-8", errors="strict"))
            except (UnicodeDecodeError, json.JSONDecodeError):
                pos = find_last_occurrence(data[:pos], key)
                continue
            found.append(raw)
            break  # occurrence terakhir untuk key ini sudah valid
    return found


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Ekstrak token persist:grita-scada-tr dari LevelDB profil Chromium."
    )
    parser.add_argument(
        "--profile",
        default=os.path.expanduser("~/.cache/scada-chromium-1"),
        help="Path profil Chromium (default: ~/.cache/scada-chromium-1)",
    )
    args = parser.parse_args()

    leveldb_dir = os.path.join(args.profile, "Default", "Local Storage", "leveldb")
    if not os.path.isdir(leveldb_dir):
        print(
            f"error: direktori LevelDB tidak ditemukan: {leveldb_dir}",
            file=sys.stderr,
        )
        return 1

    key_variants = read_key_prefix_bytes(b"persist:grita-scada-tr")
    # Fallback: cari key mentah tanpa prefix origin (bentuk lain LevelDB).
    key_variants.append(b"persist:grita-scada-tr")

    files = sorted(glob.glob(os.path.join(leveldb_dir, "000003.log")))
    files += sorted(glob.glob(os.path.join(leveldb_dir, "*.ldb")))

    candidates: list[bytes] = []
    for path in files:
        try:
            candidates.extend(scan_file(path, key_variants))
        except OSError as exc:
            print(f"warning: gagal baca {path}: {exc}", file=sys.stderr)

    if not candidates:
        print("error: token persist:grita-scada-tr tidak ditemukan", file=sys.stderr)
        return 1

    # Ambil kandidat terakhir (occurrence terbaru dalam file terakhir).
    token = candidates[-1]
    # Validasi final: harus JSON object.
    parsed = json.loads(token.decode("utf-8"))
    if not isinstance(parsed, dict):
        print("error: token bukan JSON object", file=sys.stderr)
        return 1

    # Cetak HANYA ke stdout. Jangan logging apapun yang memuat token.
    sys.stdout.write(token.decode("utf-8"))
    sys.stdout.write("\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
