package cycle

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grita/iconics-eos-monitoring/internal/portal"
	"github.com/grita/iconics-eos-monitoring/internal/portalclient"
	"github.com/grita/iconics-eos-monitoring/internal/store"
)

// fakeSocket meniru PembacaPortalSocket: jumlah panggilan, token yang dipakai,
// dan error per panggilan bisa diatur test.
type fakeSocket struct {
	mu            sync.Mutex
	fetchCalls    int
	collectCalls  int
	fetchTokens   []string
	collectTokens []string
	fetchErr      []error // error per indeks panggilan; habis berarti sukses
	collectErr    []error
	clients       []portalclient.Client
	logs          map[int]portalclient.LogStat
	// collectOpts merekam opsi yang diterima CollectLogs supaya penerusan
	// jendela/idle dari Cycle bisa diuji, bukan hanya dibaca helper-nya.
	collectOpts []portalclient.CollectOptions
}

func (f *fakeSocket) FetchClients(_ context.Context, token string) ([]portalclient.Client, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.fetchCalls
	f.fetchCalls++
	f.fetchTokens = append(f.fetchTokens, token)
	if i < len(f.fetchErr) && f.fetchErr[i] != nil {
		return nil, f.fetchErr[i]
	}
	return f.clients, nil
}

func (f *fakeSocket) CollectLogs(_ context.Context, token string, opt portalclient.CollectOptions) (map[int]portalclient.LogStat, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.collectCalls
	f.collectCalls++
	f.collectTokens = append(f.collectTokens, token)
	f.collectOpts = append(f.collectOpts, opt)
	if i < len(f.collectErr) && f.collectErr[i] != nil {
		return nil, f.collectErr[i]
	}
	return f.logs, nil
}

func (f *fakeSocket) jumlahFetch() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fetchCalls
}

func (f *fakeSocket) tokenFetch() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.fetchTokens...)
}

// opsiCollect mengembalikan salinan opsi yang diterima CollectLogs.
func (f *fakeSocket) opsiCollect() []portalclient.CollectOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]portalclient.CollectOptions(nil), f.collectOpts...)
}

// newCycleLogin merakit Cycle mode socket dengan store in-memory dan logger
// senyap, cukup untuk menguji collectPortalSocket secara langsung.
func newCycleLogin(t *testing.T, socket PembacaPortalSocket, token string,
	login func(context.Context) (string, error)) (*Cycle, *store.Store) {
	t.Helper()
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	st, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("buka sqlite: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	return &Cycle{
		Store:           st,
		Socket:          socket,
		Log:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		PortalMode:      PortalModeSocket,
		PortalToken:     token,
		PortalWindow:    5 * time.Millisecond,
		PortalMinWindow: time.Millisecond,
		LoginPortal:     login,
	}, st
}

// jwtExp membuat JWT tiruan dengan klaim exp tertentu.
func jwtExp(t *testing.T, exp time.Time) string {
	t.Helper()
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp.Unix())))
	return "head." + payload + ".sig"
}

func satuKlien() []portalclient.Client {
	return []portalclient.Client{
		{RTUID: 12012000, StatusMQTT: true, Station: portalclient.Station{Name: "UPS.01.FUY"}},
	}
}

// TestSweepLoginSaatTokenKedaluwarsa: token mati disegarkan lewat login, lalu
// sweep memakai token baru dan token itu tersimpan di store.
func TestSweepLoginSaatTokenKedaluwarsa(t *testing.T) {
	oldToken := jwtExp(t, time.Now().Add(-time.Hour))
	newToken := jwtExp(t, time.Now().Add(8*time.Hour))
	fake := &fakeSocket{clients: satuKlien()}

	loginCalls := 0
	login := func(context.Context) (string, error) {
		loginCalls++
		return newToken, nil
	}
	c, st := newCycleLogin(t, fake, oldToken, login)

	if _, err := c.collectPortalSocket(context.Background()); err != nil {
		t.Fatalf("collectPortalSocket: %v", err)
	}
	if loginCalls != 1 {
		t.Fatalf("login dipanggil %d kali, mau 1", loginCalls)
	}
	tokens := fake.tokenFetch()
	if len(tokens) != 1 || tokens[0] != newToken {
		t.Fatalf("FetchClients dipanggil dengan %v, mau token baru", tokens)
	}
	ps, err := st.GetPortalSession(context.Background())
	if err != nil {
		t.Fatalf("GetPortalSession: %v", err)
	}
	if ps.Token != newToken {
		t.Errorf("token tersimpan = %q, mau token baru", ps.Token)
	}
	if !ps.Valid {
		t.Errorf("token hasil login harus valid=1")
	}
}

// TestSweepTanpaLoginSaatTokenSegar: token masih berlaku, jadi login tidak
// dipanggil dan sweep langsung memakai token lama.
func TestSweepTanpaLoginSaatTokenSegar(t *testing.T) {
	segar := jwtExp(t, time.Now().Add(8*time.Hour))
	fake := &fakeSocket{clients: satuKlien()}

	loginCalls := 0
	login := func(context.Context) (string, error) {
		loginCalls++
		return "head.baru.sig", nil
	}
	c, _ := newCycleLogin(t, fake, segar, login)

	if _, err := c.collectPortalSocket(context.Background()); err != nil {
		t.Fatalf("collectPortalSocket: %v", err)
	}
	if loginCalls != 0 {
		t.Fatalf("login dipanggil %d kali, mau 0 (token masih segar)", loginCalls)
	}
	tokens := fake.tokenFetch()
	if len(tokens) != 1 || tokens[0] != segar {
		t.Fatalf("FetchClients dipanggil dengan %v, mau token lama", tokens)
	}
}

// TestSweepLoginSaatTokenDitolak: token ditolak portal -> login sekali, sweep
// diulang sekali, dan siklus berhasil.
func TestSweepLoginSaatTokenDitolak(t *testing.T) {
	segar := jwtExp(t, time.Now().Add(8*time.Hour))
	newToken := jwtExp(t, time.Now().Add(8*time.Hour))
	fake := &fakeSocket{
		clients:  satuKlien(),
		fetchErr: []error{portalclient.ErrUnauthorized},
	}

	loginCalls := 0
	login := func(context.Context) (string, error) {
		loginCalls++
		return newToken, nil
	}
	c, _ := newCycleLogin(t, fake, segar, login)

	if _, err := c.collectPortalSocket(context.Background()); err != nil {
		t.Fatalf("collectPortalSocket: %v", err)
	}
	if loginCalls != 1 {
		t.Fatalf("login dipanggil %d kali, mau 1", loginCalls)
	}
	if fake.jumlahFetch() != 2 {
		t.Fatalf("FetchClients dipanggil %d kali, mau 2 (awal + ulang)", fake.jumlahFetch())
	}
	tokens := fake.tokenFetch()
	if tokens[1] != newToken {
		t.Errorf("percobaan ulang memakai token %q, mau token baru", tokens[1])
	}
}

// TestSweepTokenDitolakLagi: portal tetap menolak setelah login -> jalur sesi
// kedaluwarsa, dan login tidak diulang lebih dari sekali.
func TestSweepTokenDitolakLagi(t *testing.T) {
	segar := jwtExp(t, time.Now().Add(8*time.Hour))
	fake := &fakeSocket{
		clients:  satuKlien(),
		fetchErr: []error{portalclient.ErrUnauthorized, portalclient.ErrUnauthorized, portalclient.ErrUnauthorized},
	}

	loginCalls := 0
	login := func(context.Context) (string, error) {
		loginCalls++
		return jwtExp(t, time.Now().Add(8*time.Hour)), nil
	}
	c, _ := newCycleLogin(t, fake, segar, login)

	_, err := c.collectPortalSocket(context.Background())
	if !errorsIs(err, portal.ErrSessionExpired) {
		t.Fatalf("error = %v, mau portal.ErrSessionExpired", err)
	}
	if loginCalls != 1 {
		t.Fatalf("login dipanggil %d kali, mau 1", loginCalls)
	}
	if fake.jumlahFetch() != 2 {
		t.Fatalf("FetchClients dipanggil %d kali, mau 2 lalu menyerah", fake.jumlahFetch())
	}
}

// TestSweepLoginSudahLoginTanpaCooldown: portal menolak karena akun masih
// punya sesi aktif. Itu bukan kesalahan kredensial, jadi siklus TIDAK boleh
// memasang cooldown 6 jam — cukup log dan lanjut dengan token lama.
func TestSweepLoginSudahLoginTanpaCooldown(t *testing.T) {
	oldToken := jwtExp(t, time.Now().Add(-time.Hour))
	fake := &fakeSocket{clients: satuKlien()}
	login := func(context.Context) (string, error) {
		return "", fmt.Errorf("portalclient: verifikasi 2FA ditolak (HTTP 401): "+
			"User is already logged in: %w", portalclient.ErrSudahLogin)
	}
	c, st := newCycleLogin(t, fake, oldToken, login)
	c.LoginCooldown = time.Hour

	if _, err := c.collectPortalSocket(context.Background()); err != nil {
		t.Fatalf("collectPortalSocket: %v", err)
	}
	_, _, ok, err := st.LoginCooldown(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("LoginCooldown: %v", err)
	}
	if ok {
		t.Errorf("cooldown terpasang padahal akun hanya masih punya sesi aktif")
	}
}

// TestSweepPakaiTokenTerbaruDariStore: token segar yang ditulis jalur lain
// (POST /api/session/login, PUT /api/session/token, push_token.py) harus dipakai
// siklus. Sebelum perbaikan, siklus memakai token mati di memori lalu melaporkan
// "sesi kedaluwarsa" dan menahan laporan padahal token baru sudah tersimpan.
func TestSweepPakaiTokenTerbaruDariStore(t *testing.T) {
	mati := jwtExp(t, time.Now().Add(-time.Hour))
	segar := jwtExp(t, time.Now().Add(8*time.Hour))
	fake := &fakeSocket{clients: satuKlien()}
	loginCalls := 0
	login := func(context.Context) (string, error) {
		loginCalls++
		return "head.login.sig", nil
	}
	c, st := newCycleLogin(t, fake, mati, login)
	if err := st.SavePortalToken(context.Background(), segar, true); err != nil {
		t.Fatalf("SavePortalToken: %v", err)
	}

	if _, err := c.collectPortalSocket(context.Background()); err != nil {
		t.Fatalf("collectPortalSocket: %v", err)
	}
	if loginCalls != 0 {
		t.Errorf("login dipanggil %d kali; token segar dari store harus dipakai", loginCalls)
	}
	tokens := fake.tokenFetch()
	if len(tokens) != 1 || tokens[0] != segar {
		t.Errorf("FetchClients dipakai %v, mau token dari store", tokens)
	}
}

// TestNewMeneruskanIdleTimeoutDariEnv: PORTAL_LOG_IDLE_SECONDS harus sampai ke
// struct Cycle lewat konstruktor New, bukan hanya terbaca helper-nya.
func TestNewMeneruskanIdleTimeoutDariEnv(t *testing.T) {
	t.Setenv("PORTAL_LOG_IDLE_SECONDS", "90")
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
	st, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("buka sqlite: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	c, err := New(st, "token", "http://wa", "kunci", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.PortalIdle != 90*time.Second {
		t.Errorf("PortalIdle = %s, mau 90s", c.PortalIdle)
	}
}

// TestSweepMeneruskanIdleTimeout: opsi idle yang dipegang Cycle harus benar
// diteruskan ke pembaca portal saat pengumpulan; tanpa itu RTU kronis yang
// tidak pernah mengirim tetap menahan jendela maksimum penuh.
func TestSweepMeneruskanIdleTimeout(t *testing.T) {
	fake := &fakeSocket{clients: satuKlien()}
	login := func(context.Context) (string, error) { return "", fmt.Errorf("tidak dipakai") }
	c, _ := newCycleLogin(t, fake, jwtExp(t, time.Now().Add(time.Hour)), login)
	c.PortalIdle = 77 * time.Second

	if _, err := c.collectPortalSocket(context.Background()); err != nil {
		t.Fatalf("collectPortalSocket: %v", err)
	}
	opts := fake.opsiCollect()
	if len(opts) == 0 {
		t.Fatal("CollectLogs tidak dipanggil")
	}
	if opts[0].IdleTimeout != 77*time.Second {
		t.Errorf("IdleTimeout yang diterima pembaca = %s, mau 77s", opts[0].IdleTimeout)
	}
	if opts[0].MinWindow != c.PortalMinWindow || opts[0].MaxWindow != c.PortalWindow {
		t.Errorf("jendela tidak diteruskan: min=%s maks=%s", opts[0].MinWindow, opts[0].MaxWindow)
	}
}

// fakeSocketSumber menambahkan pelaporan asal daftar klien pada fakeSocket,
// meniru *portalclient.Reader (socket live vs cadangan REST).
type fakeSocketSumber struct {
	fakeSocket
	sumber string
}

func (f *fakeSocketSumber) SumberKlienTerakhir() string { return f.sumber }

// TestTelemetriSahBergantungSumberKlien: pengiriman hanya ditahan bila daftar
// klien datang dari cadangan REST (tanpa status broker). RTU yang diam TIDAK
// menahan pengiriman — itu bucket "no data from broker" di laporan.
func TestTelemetriSahBergantungSumberKlien(t *testing.T) {
	kasus := []struct {
		nama   string
		socket PembacaPortalSocket
		mau    bool
	}{
		{"socket live", &fakeSocketSumber{sumber: portalclient.SumberSocket}, true},
		{"cadangan REST", &fakeSocketSumber{sumber: portalclient.SumberREST}, false},
		{"tanpa info sumber", &fakeSocket{}, true},
	}
	for _, k := range kasus {
		c := &Cycle{Socket: k.socket, PortalMode: PortalModeSocket}
		if got := c.telemetriSah(); got != k.mau {
			t.Errorf("%s: telemetriSah() = %v, mau %v", k.nama, got, k.mau)
		}
	}
	// Mode browser tidak memakai aturan ini.
	c := &Cycle{PortalMode: PortalModeBrowser}
	if !c.telemetriSah() {
		t.Errorf("mode browser: telemetriSah() harus true")
	}
}

// TestSweepTanpaLoginPortal: login tidak dikonfigurasi -> perilaku lama, tanpa
// panic, walau token kosong.
func TestSweepTanpaLoginPortal(t *testing.T) {
	fake := &fakeSocket{clients: satuKlien()}
	c, _ := newCycleLogin(t, fake, "", nil)

	if _, err := c.collectPortalSocket(context.Background()); err != nil {
		t.Fatalf("collectPortalSocket tanpa login: %v", err)
	}
	if fake.jumlahFetch() != 1 {
		t.Fatalf("FetchClients dipanggil %d kali, mau 1", fake.jumlahFetch())
	}
}

// TestSweepCooldownAktifTidakLogin: cooldown aktif menahan login otomatis;
// sweep tetap berjalan memakai token lama tanpa menghubungi portal auth.
func TestSweepCooldownAktifTidakLogin(t *testing.T) {
	ctx := context.Background()
	oldToken := jwtExp(t, time.Now().Add(-time.Hour))
	fake := &fakeSocket{clients: satuKlien()}

	loginCalls := 0
	login := func(context.Context) (string, error) {
		loginCalls++
		return jwtExp(t, time.Now().Add(8*time.Hour)), nil
	}
	c, st := newCycleLogin(t, fake, oldToken, login)
	c.LoginCooldown = time.Hour
	if err := st.CatatLoginGagal(ctx, time.Now().Add(time.Hour), "gagal sebelumnya"); err != nil {
		t.Fatalf("seed cooldown: %v", err)
	}

	if _, err := c.collectPortalSocket(ctx); err != nil {
		t.Fatalf("collectPortalSocket: %v", err)
	}
	if loginCalls != 0 {
		t.Fatalf("login dipanggil %d kali, mau 0 (cooldown aktif)", loginCalls)
	}
	tokens := fake.tokenFetch()
	if len(tokens) != 1 || tokens[0] != oldToken {
		t.Fatalf("FetchClients token %v, mau token lama", tokens)
	}
}

// TestSweepLoginGagalCatatCooldown: kegagalan login mencatat cooldown di store
// dengan sampai sekitar sekarang + LoginCooldown.
func TestSweepLoginGagalCatatCooldown(t *testing.T) {
	ctx := context.Background()
	oldToken := jwtExp(t, time.Now().Add(-time.Hour))
	fake := &fakeSocket{clients: satuKlien()}

	loginCalls := 0
	login := func(context.Context) (string, error) {
		loginCalls++
		return "", errors.New("portalclient: login ditolak portal")
	}
	c, st := newCycleLogin(t, fake, oldToken, login)
	c.LoginCooldown = 90 * time.Minute

	start := time.Now()
	if _, err := c.collectPortalSocket(ctx); err != nil {
		t.Fatalf("collectPortalSocket: %v", err)
	}
	if loginCalls != 1 {
		t.Fatalf("login dipanggil %d kali, mau 1", loginCalls)
	}
	sampai, _, ok, err := st.LoginCooldown(ctx, time.Now())
	if err != nil || !ok {
		t.Fatalf("cooldown = ok %v err %v, mau true/nil", ok, err)
	}
	want := start.Add(90 * time.Minute)
	if diff := sampai.Sub(want); diff < -time.Minute || diff > time.Minute {
		t.Fatalf("sampai = %v, mau sekitar %v", sampai, want)
	}
}

// TestSweepLoginSuksesHapusCooldown: login berhasil membersihkan cooldown
// (pesan lama benar-benar hilang, bukan sekadar lewat).
func TestSweepLoginSuksesHapusCooldown(t *testing.T) {
	ctx := context.Background()
	oldToken := jwtExp(t, time.Now().Add(-time.Hour))
	newToken := jwtExp(t, time.Now().Add(8*time.Hour))
	fake := &fakeSocket{clients: satuKlien()}

	login := func(context.Context) (string, error) { return newToken, nil }
	c, st := newCycleLogin(t, fake, oldToken, login)
	c.LoginCooldown = time.Hour
	if err := st.CatatLoginGagal(ctx, time.Now().Add(-time.Hour), "gagal lama"); err != nil {
		t.Fatalf("seed cooldown: %v", err)
	}

	if _, err := c.collectPortalSocket(ctx); err != nil {
		t.Fatalf("collectPortalSocket: %v", err)
	}
	_, pesan, ok, err := st.LoginCooldown(ctx, time.Now())
	if err != nil || ok || pesan != "" {
		t.Fatalf("setelah sukses = ok %v pesan %q err %v, mau false/''/nil", ok, pesan, err)
	}
}

// TestSweepCooldownBertahanLintasSweep: cooldown dari kegagalan pertama menahan
// login pada sweep berikutnya; login hanya dicoba sekali.
func TestSweepCooldownBertahanLintasSweep(t *testing.T) {
	ctx := context.Background()
	oldToken := jwtExp(t, time.Now().Add(-time.Hour))
	fake := &fakeSocket{clients: satuKlien()}

	loginCalls := 0
	login := func(context.Context) (string, error) {
		loginCalls++
		return "", errors.New("portalclient: login ditolak portal")
	}
	c, _ := newCycleLogin(t, fake, oldToken, login)
	c.LoginCooldown = time.Hour

	if _, err := c.collectPortalSocket(ctx); err != nil {
		t.Fatalf("sweep pertama: %v", err)
	}
	if _, err := c.collectPortalSocket(ctx); err != nil {
		t.Fatalf("sweep kedua: %v", err)
	}
	if loginCalls != 1 {
		t.Fatalf("login dipanggil %d kali, mau 1 (cooldown menahan sweep kedua)", loginCalls)
	}
}
