package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/grita/iconics-eos-monitoring/internal/portalclient"
	"github.com/grita/iconics-eos-monitoring/internal/store"
)

// fakeGuard memenuhi LoginGuard untuk pengujian memakai store in-memory.
type fakeGuard struct {
	st    *store.Store
	menit int
}

func (g fakeGuard) CatatLoginGagal(ctx context.Context, sampai time.Time, pesan string) error {
	return g.st.CatatLoginGagal(ctx, sampai, pesan)
}

func (g fakeGuard) HapusLoginCooldown(ctx context.Context) error { return g.st.HapusLoginCooldown(ctx) }

func (g fakeGuard) CooldownMenit() int { return g.menit }

// newLoginServer merakit Server dengan LoginPortal yang bisa diatur test.
func newLoginServer(t *testing.T, lp LoginPortal) (*Server, *store.Store) {
	t.Helper()
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	st, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("buka sqlite: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	srv := NewServer(Config{
		Store:       st,
		APIKey:      "kunci-uji",
		LoginPortal: lp,
		LoginGuard:  fakeGuard{st: st, menit: 60},
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return srv, st
}

// jwtLogin membuat JWT tiruan berisi klaim exp.
func jwtLogin(t *testing.T, exp time.Time) string {
	t.Helper()
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix())))
	return "head." + payload + ".sig"
}

// TestSessionLoginTidakDikonfigurasi: tanpa kredensial -> 409 dengan pesan
// konfigurasi, dan tetap wajib X-API-Key.
func TestSessionLoginTidakDikonfigurasi(t *testing.T) {
	srv, _ := newLoginServer(t, nil)
	h := srv.Handler()

	if rec := doJSON(t, h, http.MethodPost, "/api/session/login", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("tanpa API key = %d, mau 401", rec.Code)
	}
	rec := doJSON(t, h, http.MethodPost, "/api/session/login", "kunci-uji", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("tanpa kredensial = %d, mau 409: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "tidak dikonfigurasi") {
		t.Errorf("pesan kurang jelas: %s", rec.Body.String())
	}
}

// TestSessionLoginDuaFaktor: akun 2FA tanpa secret -> 409 dua_faktor true.
func TestSessionLoginDuaFaktor(t *testing.T) {
	srv, _ := newLoginServer(t, func(context.Context) (HasilLoginPortal, error) {
		return HasilLoginPortal{}, portalclient.ErrDuaFaktorDiperlukan
	})
	rec := doJSON(t, srv.Handler(), http.MethodPost, "/api/session/login", "kunci-uji", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("2FA = %d, mau 409: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["dua_faktor"] != true {
		t.Errorf("dua_faktor = %v, mau true", body["dua_faktor"])
	}
}

// TestSessionLoginGagal: kegagalan login lain -> 502 tanpa membocorkan detail.
func TestSessionLoginGagal(t *testing.T) {
	srv, _ := newLoginServer(t, func(context.Context) (HasilLoginPortal, error) {
		return HasilLoginPortal{}, errors.New("portalclient: request login: koneksi ditolak")
	})
	rec := doJSON(t, srv.Handler(), http.MethodPost, "/api/session/login", "kunci-uji", "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("gagal = %d, mau 502: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "login portal gagal") {
		t.Errorf("pesan = %s", rec.Body.String())
	}
}

// TestSessionLoginSukses: token tersimpan, respons OK tanpa token, dan exp
// lokal ikut dilaporkan.
func TestSessionLoginSukses(t *testing.T) {
	token := jwtLogin(t, time.Now().Add(8*time.Hour))
	srv, st := newLoginServer(t, func(context.Context) (HasilLoginPortal, error) {
		exp, _ := portalclient.TokenExpiry(token)
		return HasilLoginPortal{Token: token, Exp: exp}, nil
	})
	rec := doJSON(t, srv.Handler(), http.MethodPost, "/api/session/login", "kunci-uji", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("sukses = %d, mau 200: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["ok"] != true || body["dua_faktor"] != false {
		t.Errorf("respons = %v", body)
	}
	if _, ada := body["token_exp_local"]; !ada {
		t.Errorf("token_exp_local harus ada: %v", body)
	}
	if strings.Contains(rec.Body.String(), token) {
		t.Fatalf("token tidak boleh dikirim ke klien: %s", rec.Body.String())
	}
	ps, err := st.GetPortalSession(context.Background())
	if err != nil {
		t.Fatalf("GetPortalSession: %v", err)
	}
	if ps.Token != token || !ps.Valid {
		t.Errorf("sesi tersimpan = %+v, mau token baru valid", ps)
	}
}

// TestSessionLoginGagalCatatCooldown: kegagalan login manual mencatat cooldown
// supaya siklus terjadwal ikut berhenti mencoba.
func TestSessionLoginGagalCatatCooldown(t *testing.T) {
	srv, st := newLoginServer(t, func(context.Context) (HasilLoginPortal, error) {
		return HasilLoginPortal{}, errors.New("portalclient: request login: koneksi ditolak")
	})
	start := time.Now()
	rec := doJSON(t, srv.Handler(), http.MethodPost, "/api/session/login", "kunci-uji", "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("gagal = %d, mau 502: %s", rec.Code, rec.Body.String())
	}
	sampai, _, ok, err := st.LoginCooldown(context.Background(), time.Now())
	if err != nil || !ok {
		t.Fatalf("cooldown = ok %v err %v, mau true/nil", ok, err)
	}
	want := start.Add(60 * time.Minute)
	if diff := sampai.Sub(want); diff < -time.Minute || diff > time.Minute {
		t.Fatalf("sampai = %v, mau sekitar %v", sampai, want)
	}
}

// TestSessionLoginSudahLoginTanpaCooldown: kegagalan karena akun masih punya
// sesi aktif bukan kesalahan kredensial, jadi endpoint manual tidak boleh
// memasang cooldown 6 jam yang akan memblokir siklus setelah sesi itu berakhir.
func TestSessionLoginSudahLoginTanpaCooldown(t *testing.T) {
	srv, st := newLoginServer(t, func(context.Context) (HasilLoginPortal, error) {
		return HasilLoginPortal{}, fmt.Errorf(
			"portalclient: verifikasi 2FA ditolak (HTTP 401): User is already logged in: %w",
			portalclient.ErrSudahLogin)
	})
	rec := doJSON(t, srv.Handler(), http.MethodPost, "/api/session/login", "kunci-uji", "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("gagal = %d, mau 502: %s", rec.Code, rec.Body.String())
	}
	if _, _, ok, err := st.LoginCooldown(context.Background(), time.Now()); err != nil || ok {
		t.Fatalf("cooldown = ok %v err %v, mau false/nil (bukan kesalahan kredensial)", ok, err)
	}
}

// TestSessionLoginSuksesHapusCooldown: login manual berhasil membersihkan
// cooldown yang tertinggal.
func TestSessionLoginSuksesHapusCooldown(t *testing.T) {
	token := jwtLogin(t, time.Now().Add(8*time.Hour))
	srv, st := newLoginServer(t, func(context.Context) (HasilLoginPortal, error) {
		exp, _ := portalclient.TokenExpiry(token)
		return HasilLoginPortal{Token: token, Exp: exp}, nil
	})
	ctx := context.Background()
	if err := st.CatatLoginGagal(ctx, time.Now().Add(-time.Hour), "gagal lama"); err != nil {
		t.Fatalf("seed cooldown: %v", err)
	}
	rec := doJSON(t, srv.Handler(), http.MethodPost, "/api/session/login", "kunci-uji", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("sukses = %d, mau 200: %s", rec.Code, rec.Body.String())
	}
	_, pesan, ok, err := st.LoginCooldown(ctx, time.Now())
	if err != nil || ok || pesan != "" {
		t.Fatalf("setelah sukses = ok %v pesan %q err %v, mau false/''/nil", ok, pesan, err)
	}
}

// TestSessionStatusMemuatCooldown: status sesi melaporkan cooldown login aktif
// beserta pesannya.
func TestSessionStatusMemuatCooldown(t *testing.T) {
	srv, st := newLoginServer(t, nil)
	ctx := context.Background()
	if err := st.CatatLoginGagal(ctx, time.Now().Add(time.Hour), "kredensial salah"); err != nil {
		t.Fatalf("seed cooldown: %v", err)
	}
	rec := doJSON(t, srv.Handler(), http.MethodGet, "/api/session/status", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, mau 200: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if s, _ := body["login_cooldown_sampai"].(string); s == "" {
		t.Errorf("login_cooldown_sampai kosong: %v", body)
	}
	if body["login_cooldown_pesan"] != "kredensial salah" {
		t.Errorf("login_cooldown_pesan = %v, mau pesan tersimpan", body["login_cooldown_pesan"])
	}
}
