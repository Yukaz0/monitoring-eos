// Package api menyediakan REST API monitoring (chi router) dan pengirim
// laporan WhatsApp via whatsapp-service dengan stagger antar pesan.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/grita/iconics-eos-monitoring/internal/portalclient"
	"github.com/grita/iconics-eos-monitoring/internal/report"
	"github.com/grita/iconics-eos-monitoring/internal/scheduler"
	"github.com/grita/iconics-eos-monitoring/internal/store"
)

const requestTimeout = 30 * time.Second

// sendTimeout adalah batas waktu pengiriman WA satu laporan setelah submit.
const sendTimeout = 2 * time.Minute

// maxRingkasLoginErr membatasi panjang ringkasan kegagalan login yang dicatat
// sebagai cooldown (bukan pesan penuh).
const maxRingkasLoginErr = 200

// RunCycler adalah siklus laporan penuh (collector -> render -> simpan ->
// kirim WA) yang dipicu POST /api/report/run dan scheduler.
type RunCycler func(ctx context.Context)

// ProgressProvider melaporkan kemajuan pengumpulan traffic RTU pada siklus
// yang sedang berjalan: berapa RTU sudah mengirim, berapa yang ditunggu, dan
// apakah fase pengumpulan masih aktif. Dipenuhi *cycle.Cycle dan dipakai
// GET /api/report/status supaya progress bar dashboard punya data nyata.
type ProgressProvider interface {
	Progress() (collected, expected int, aktif bool)
}

// HasilLoginPortal merangkum hasil login REST portal untuk endpoint sesi.
type HasilLoginPortal struct {
	Token     string
	DuaFaktor bool
	// Exp nol berarti exp token tidak terbaca dari JWT.
	Exp time.Time
}

// LoginPortal menjalankan login REST portal memakai kredensial konfigurasi.
// Nil berarti login tidak dikonfigurasi (env PORTAL_USERNAME/PORTAL_PASSWORD
// kosong); endpoint POST /api/session/login membalas 409.
type LoginPortal func(ctx context.Context) (HasilLoginPortal, error)

// LoginGuard menghubungkan API ke cooldown login persisten tanpa membuat
// package api bergantung pada internal/cycle: cmd/api mengisinya dari
// *store.Store (penyimpanan) + *cycle.Cycle (durasi cooldown). Dipakai
// POST /api/session/login supaya siklus terjadwal ikut berhenti mencoba
// setelah operator gagal login manual.
type LoginGuard interface {
	CatatLoginGagal(ctx context.Context, sampai time.Time, pesan string) error
	HapusLoginCooldown(ctx context.Context) error
	CooldownMenit() int
}

// Server menyimpan dependensi REST API.
type Server struct {
	st       *store.Store
	apiKey   string
	waBase   string
	waAPIKey string
	waHTTP   *http.Client
	log      *slog.Logger
	sched    *scheduler.Scheduler
	cycle    RunCycler
	progress ProgressProvider
	login    LoginPortal
	guard    LoginGuard
	tracker  *CycleTracker

	cycleMu sync.Mutex // satu siklus laporan dalam satu waktu
}

// Config adalah opsi pembuatan Server.
type Config struct {
	Store    *store.Store
	APIKey   string // API key internal (header X-API-Key)
	WABase   string // base URL whatsapp-service, mis. http://whatsapp-service:3101
	WAAPIKey string // API key whatsapp-service (header X-API-Key)
	Sched    *scheduler.Scheduler
	Cycle    RunCycler
	Log      *slog.Logger
	// Tracker mencatat status siklus untuk GET /api/report/status. Bila nil,
	// Server membuat tracker sendiri (cocok untuk test).
	Tracker *CycleTracker
	// Progress melaporkan kemajuan pengumpulan traffic RTU (progress bar).
	// Boleh nil: respons status hanya kehilangan field rtu_terkumpul.
	Progress ProgressProvider
	// LoginPortal menjalankan login REST portal untuk POST /api/session/login.
	// Boleh nil: endpoint membalas 409 (login tidak dikonfigurasi).
	LoginPortal LoginPortal
	// LoginGuard mencatat/membersihkan cooldown login persisten. Boleh nil:
	// endpoint tetap bekerja tanpa menghentikan siklus terjadwal.
	LoginGuard LoginGuard
}

// NewServer membuat Server dengan HTTP client timeout untuk panggilan WA.
func NewServer(cfg Config) *Server {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Tracker == nil {
		cfg.Tracker = &CycleTracker{}
	}
	return &Server{
		st:       cfg.Store,
		apiKey:   cfg.APIKey,
		waBase:   cfg.WABase,
		waAPIKey: cfg.WAAPIKey,
		waHTTP:   &http.Client{Timeout: 60 * time.Second},
		log:      cfg.Log,
		sched:    cfg.Sched,
		cycle:    cfg.Cycle,
		progress: cfg.Progress,
		login:    cfg.LoginPortal,
		guard:    cfg.LoginGuard,
		tracker:  cfg.Tracker,
	}
}

// Handler merakit semua route. CORS middleware mengizinkan origin dashboard
// frontend (localhost:5117 dan origin sama-sama-IP:5117) memanggil API.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(s.corsMiddleware)

	r.Get("/api/health", s.handleHealth)

	r.Route("/api/recipients", func(r chi.Router) {
		r.Get("/", s.handleListRecipients)
		r.Post("/", s.requireAPIKey(s.handleCreateRecipient))
		r.Put("/{id}", s.requireAPIKey(s.handleUpdateRecipient))
		r.Delete("/{id}", s.requireAPIKey(s.handleDeleteRecipient))
	})

	r.Route("/api/schedules", func(r chi.Router) {
		r.Get("/", s.handleListSchedules)
		r.Put("/", s.requireAPIKey(s.handlePutSchedule))
	})

	r.Get("/api/report/latest", s.handleLatestReport)
	r.Get("/api/report/history", s.handleReportHistory)
	r.Get("/api/report/status", s.handleReportStatus)
	r.Post("/api/report/run", s.requireAPIKey(s.handleReportRun))
	// Untuk collector standalone: kirim laporan jadi tanpa memicu siklus baru.
	r.Post("/api/report/submit", s.requireAPIKey(s.handleReportSubmit))

	r.Get("/api/session/status", s.handleSessionStatus)
	r.Put("/api/session/token", s.requireAPIKey(s.handleSetSessionToken))
	r.Post("/api/session/login", s.requireAPIKey(s.handleSessionLogin))

	r.Get("/api/wa/status", s.handleWAStatus)
	r.Get("/api/qr", s.requireAPIKey(s.handleWAQR))
	r.Post("/api/wa/logout", s.requireAPIKey(s.handleWALogout))
	return r
}

// corsMiddleware mengizinkan origin dashboard (localhost:5117 dan
// http://<IP-host-yang-sama>:5117). Origin kosong (curl, healthcheck) lolos
// tanpa header CORS. Preflight OPTIONS selalu 204.
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		origin := req.Header.Get("Origin")
		if origin != "" && corsAllowed(origin, req.Host) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
		}
		if req.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "X-API-Key, Content-Type")
			w.Header().Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, req)
	})
}

// corsAllowed: origin localhost:5117, atau origin http://<HOST>:5117 dengan
// HOST sama persis dengan host request ini (tanpa port).
func corsAllowed(origin, host string) bool {
	if origin == "http://localhost:5117" || origin == "http://127.0.0.1:5117" {
		return true
	}
	const suf = ":5117"
	if !strings.HasSuffix(origin, "http://"+suf) {
		if !strings.HasSuffix(origin, suf) || !strings.HasPrefix(origin, "http://") {
			return false
		}
	}
	// origin = "http://<host>:5117"
	o := strings.TrimPrefix(strings.TrimSuffix(origin, suf), "http://")
	if hi := strings.LastIndex(host, ":"); hi >= 0 {
		host = host[:hi]
	}
	return o != "" && o == host
}

// requireAPIKey memvalidasi header X-API-Key untuk endpoint mutasi.
func (s *Server) requireAPIKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if s.apiKey == "" {
			http.Error(w, "server belum dikonfigurasi API_KEY", http.StatusServiceUnavailable)
			return
		}
		if req.Header.Get("X-API-Key") != s.apiKey {
			http.Error(w, "API key tidak valid", http.StatusUnauthorized)
			return
		}
		next(w, req)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleListRecipients(w http.ResponseWriter, req *http.Request) {
	// Nilai 1 di-query param only_active=1: hanya recipient aktif.
	onlyActive := req.URL.Query().Get("only_active") == "1"
	list, err := s.st.ListRecipients(req.Context(), onlyActive)
	if err != nil {
		s.internalError(w, "baca recipients", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCreateRecipient(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Name   string `json:"name"`
		Phone  string `json:"phone"`
		Active *bool  `json:"active,omitempty"`
	}
	if !s.decode(w, req, &body) {
		return
	}
	if body.Phone == "" {
		http.Error(w, "phone wajib diisi", http.StatusBadRequest)
		return
	}
	active := true
	if body.Active != nil {
		active = *body.Active
	}
	r, err := s.st.CreateRecipient(req.Context(), body.Name, body.Phone, active)
	if err != nil {
		s.internalError(w, "buat recipient", err)
		return
	}
	writeJSON(w, http.StatusCreated, r)
}

func (s *Server) handleUpdateRecipient(w http.ResponseWriter, req *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(req, "id"), 10, 64)
	if err != nil {
		http.Error(w, "id tidak valid", http.StatusBadRequest)
		return
	}
	var body struct {
		Name   string `json:"name"`
		Phone  string `json:"phone"`
		Active *bool  `json:"active,omitempty"`
	}
	if !s.decode(w, req, &body) {
		return
	}
	r, err := s.st.UpdateRecipient(req.Context(), id, body.Name, body.Phone, body.Active)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "recipient tidak ditemukan", http.StatusNotFound)
		return
	}
	if err != nil {
		s.internalError(w, "update recipient", err)
		return
	}
	writeJSON(w, http.StatusOK, r)
}

func (s *Server) handleDeleteRecipient(w http.ResponseWriter, req *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(req, "id"), 10, 64)
	if err != nil {
		http.Error(w, "id tidak valid", http.StatusBadRequest)
		return
	}
	err = s.st.DeleteRecipient(req.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "recipient tidak ditemukan", http.StatusNotFound)
		return
	}
	if err != nil {
		s.internalError(w, "hapus recipient", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) handleListSchedules(w http.ResponseWriter, req *http.Request) {
	list, err := s.st.ListSchedules(req.Context(), false)
	if err != nil {
		s.internalError(w, "baca schedules", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handlePutSchedule(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Name     string `json:"name"`
		CronExpr string `json:"cron_expr"`
		Active   *bool  `json:"active,omitempty"`
	}
	if !s.decode(w, req, &body) {
		return
	}
	if body.Name == "" || body.CronExpr == "" {
		http.Error(w, "name dan cron_expr wajib diisi", http.StatusBadRequest)
		return
	}
	active := true
	if body.Active != nil {
		active = *body.Active
	}
	sc, err := s.st.UpsertSchedule(req.Context(), body.Name, body.CronExpr, active)
	if err != nil {
		s.internalError(w, "simpan schedule", err)
		return
	}
	// Muat ulang entry cron agar jadwal baru langsung berlaku.
	if s.sched != nil {
		if err := s.sched.Reload(req.Context()); err != nil {
			s.log.Warn("reload scheduler setelah upsert schedule", "err", err)
		}
	}
	writeJSON(w, http.StatusOK, sc)
}

func (s *Server) handleLatestReport(w http.ResponseWriter, req *http.Request) {
	r, err := s.st.LatestReport(req.Context())
	if errors.Is(err, store.ErrNotFound) {
		http.Error(w, "belum ada laporan", http.StatusNotFound)
		return
	}
	if err != nil {
		s.internalError(w, "baca laporan terbaru", err)
		return
	}
	writeJSON(w, http.StatusOK, r)
}

func (s *Server) handleReportHistory(w http.ResponseWriter, req *http.Request) {
	limit, err := strconv.Atoi(req.URL.Query().Get("limit"))
	if err != nil || limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	list, err := s.st.ListReports(req.Context(), limit)
	if err != nil {
		s.internalError(w, "baca riwayat laporan", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// handleReportStatus melaporkan status siklus laporan untuk dashboard. Siklus
// berjalan di background setelah POST /api/report/run (202): 60 detik bila
// semua RTU aktif, sampai jendela maksimum bila ada RTU yang diam. Dashboard
// memakai ini untuk spinner dan perkiraan selesai, bukan menebak dari jam lokal.
func (s *Server) handleReportStatus(w http.ResponseWriter, req *http.Request) {
	running, started, lastDur := s.tracker.Status()
	minDetik, maksDetik := jendelaSweep()
	resp := map[string]any{
		"running":               running,
		"min_detik":             minDetik,
		"maks_detik":            maksDetik,
		"siklus_terakhir_detik": int(lastDur.Seconds()),
	}
	if running {
		lewat := int(time.Since(started).Seconds())
		resp["mulai"] = started.UTC()
		resp["berjalan_detik"] = lewat
		// Perkiraan konservatif: jendela maksimum + 30 detik render & kirim.
		sisa := maksDetik + 30 - lewat
		if sisa < 0 {
			sisa = 0
		}
		resp["perkiraan_sisa_detik"] = sisa
		resp["fase"] = "menyusun_dan_mengirim"
		if s.progress != nil {
			collected, expected, mengumpulkan := s.progress.Progress()
			resp["rtu_terkumpul"] = collected
			resp["rtu_diharapkan"] = expected
			if mengumpulkan {
				resp["fase"] = "mengumpulkan_traffic"
			}
			if expected > 0 {
				// 100% hanya pantas muncul saat siklus benar-benar selesai.
				persen := collected * 100 / expected
				if persen > 99 {
					persen = 99
				}
				resp["persen"] = persen
			}
		}
	}
	if r, err := s.st.LatestReport(req.Context()); err == nil {
		resp["report_id"] = r.ID
		resp["generated_at"] = r.GeneratedAt
	}
	writeJSON(w, http.StatusOK, resp)
}

// jendelaSweep membaca batas jendela pengumpulan log portal (detik) yang
// dipakai cycle, agar dashboard bisa memperkirakan lama satu siklus.
func jendelaSweep() (minDetik, maksDetik int) {
	minDetik = envIntOrAPI("PORTAL_LOG_MIN_WINDOW_SECONDS", 60)
	maksDetik = envIntOrAPI("PORTAL_LOG_WINDOW_SECONDS", 600)
	if maksDetik < minDetik {
		maksDetik = minDetik
	}
	return minDetik, maksDetik
}

func envIntOrAPI(key string, fallback int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key))); err == nil && v > 0 {
		return v
	}
	return fallback
}

func (s *Server) handleReportRun(w http.ResponseWriter, req *http.Request) {
	if s.cycle == nil {
		http.Error(w, "siklus laporan belum dikonfigurasi", http.StatusServiceUnavailable)
		return
	}
	// Satu siklus dalam satu waktu; request kedua saat berjalan ditolak.
	if !s.cycleMu.TryLock() {
		http.Error(w, "siklus laporan sedang berjalan", http.StatusConflict)
		return
	}
	// Async: POST langsung 202 dan siklus jalan di background.
	go func() {
		defer s.cycleMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		s.log.Info("siklus laporan manual dipicu")
		s.cycle(ctx)
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "triggered"})
}

func (s *Server) handleReportSubmit(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Content string `json:"content"`
	}
	if !s.decode(w, req, &body) {
		return
	}
	if body.Content == "" {
		http.Error(w, "content wajib diisi", http.StatusBadRequest)
		return
	}
	id, err := s.st.SaveReport(req.Context(), time.Now(), body.Content)
	if err != nil {
		s.internalError(w, "simpan laporan", err)
		return
	}
	// Opsi A: laporan dari host (scripts/run_report_host.py) masuk lewat
	// endpoint ini; setelah tersimpan, langsung kirim ke recipient aktif
	// lewat whatsapp-service agar WA keluar dari container.
	sctx, scancel := context.WithTimeout(context.Background(), sendTimeout)
	defer scancel()
	s.SendReportToRecipients(sctx, id, body.Content)
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func (s *Server) handleSessionStatus(w http.ResponseWriter, req *http.Request) {
	ps, err := s.st.GetPortalSession(req.Context())
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusOK, map[string]any{
			"valid":                 false,
			"login_cooldown_sampai": "",
			"login_cooldown_pesan":  "",
		})
		return
	}
	if err != nil {
		s.internalError(w, "baca sesi portal", err)
		return
	}
	resp := map[string]any{
		"valid":      ps.Valid,
		"updated_at": ps.UpdatedAt,
	}
	// exp token dipakai pemantau host (scripts/token_watch.py) untuk memutuskan
	// apakah token di profil Chromium lebih baru dan perlu didorong.
	if exp, ok := portalclient.TokenExpiry(ps.Token); ok {
		resp["token_exp"] = exp.Unix()
		resp["token_exp_local"] = exp.Local().Format("2006-01-02 15:04 MST")
		resp["token_sisa_menit"] = int(time.Until(exp).Minutes())
	}
	// Cooldown login otomatis (dari kegagalan siklus terjadwal) dilaporkan
	// supaya operator tahu kenapa penyegaran token tidak berjalan.
	sampai, pesan, aktif, cerr := s.st.LoginCooldown(req.Context(), time.Now())
	if cerr != nil {
		s.internalError(w, "baca cooldown login portal", cerr)
		return
	}
	resp["login_cooldown_sampai"] = ""
	if aktif {
		resp["login_cooldown_sampai"] = sampai.Local().Format("2006-01-02 15:04 MST")
	}
	resp["login_cooldown_pesan"] = pesan
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleSetSessionToken(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if !s.decode(w, req, &body) {
		return
	}
	if body.Token == "" {
		http.Error(w, "token wajib diisi", http.StatusBadRequest)
		return
	}
	// Token baru belum diverifikasi collector; valid=0 sampai siklus berikutnya.
	if err := s.st.SavePortalToken(req.Context(), body.Token, false); err != nil {
		s.internalError(w, "simpan token portal", err)
		return
	}
	s.log.Info("token portal diperbarui via API")
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// handleSessionLogin memicu login REST portal otomatis memakai kredensial
// konfigurasi, lalu menyimpan token hasilnya. Token tidak pernah dikirim ke
// klien: respons hanya memuat boolean dan waktu kedaluwarsa lokal.
func (s *Server) handleSessionLogin(w http.ResponseWriter, req *http.Request) {
	if s.login == nil {
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok":    false,
			"pesan": "login portal tidak dikonfigurasi (PORTAL_USERNAME/PORTAL_PASSWORD)",
		})
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), requestTimeout)
	defer cancel()
	hasil, err := s.login(ctx)
	if errors.Is(err, portalclient.ErrDuaFaktorDiperlukan) {
		s.catatCooldownLogin(ctx, err)
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok":         false,
			"dua_faktor": true,
			"pesan":      "akun ini butuh kode 2FA",
		})
		return
	}
	if err != nil {
		// Pesan server boleh diringkas, tetapi tidak boleh memuat kredensial.
		s.log.Warn("login portal via API gagal", "err", err)
		s.catatCooldownLogin(ctx, err)
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"ok":    false,
			"pesan": "login portal gagal",
		})
		return
	}
	if hasil.DuaFaktor || hasil.Token == "" {
		s.catatCooldownLogin(ctx, errors.New("akun butuh kode 2FA"))
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok":         false,
			"dua_faktor": true,
			"pesan":      "akun ini butuh kode 2FA",
		})
		return
	}
	// valid=1: token baru saja diterbitkan portal, belum perlu diverifikasi
	// ulang oleh siklus.
	if err := s.st.SavePortalToken(req.Context(), hasil.Token, true); err != nil {
		s.internalError(w, "simpan token hasil login portal", err)
		return
	}
	s.bersihkanCooldownLogin(ctx)
	resp := map[string]any{"ok": true, "dua_faktor": false}
	if !hasil.Exp.IsZero() {
		resp["token_exp_local"] = hasil.Exp.Local().Format("2006-01-02 15:04 MST")
	}
	s.log.Info("login portal via API berhasil")
	writeJSON(w, http.StatusOK, resp)
}

// catatCooldownLogin mencatat cooldown kegagalan login manual ke store supaya
// siklus terjadwal ikut berhenti mencoba. Aman dipanggil saat guard nil.
// Pesan diringkas dan tidak memuat kredensial.
//
// Kegagalan karena akun masih punya sesi aktif ("User is already logged in")
// TIDAK memasang cooldown: itu bukan kesalahan kredensial, dan cooldown 6 jam
// hanya akan memblokir siklus berikutnya setelah sesi lama berakhir sendiri.
func (s *Server) catatCooldownLogin(ctx context.Context, err error) {
	if s.guard == nil {
		return
	}
	if errors.Is(err, portalclient.ErrSudahLogin) {
		s.log.Warn("login manual dilewati: akun portal masih punya sesi aktif; cooldown tidak dipasang",
			"err", err)
		return
	}
	sampai := time.Now().Add(time.Duration(s.guard.CooldownMenit()) * time.Minute)
	pesan := ""
	if err != nil {
		pesan = err.Error()
		if runes := []rune(pesan); len(runes) > maxRingkasLoginErr {
			pesan = string(runes[:maxRingkasLoginErr])
		}
	}
	if e := s.guard.CatatLoginGagal(ctx, sampai, pesan); e != nil {
		s.log.Warn("catat cooldown login portal gagal", "err", e)
	}
}

// bersihkanCooldownLogin membersihkan cooldown setelah login manual berhasil.
// Aman dipanggil saat guard nil.
func (s *Server) bersihkanCooldownLogin(ctx context.Context) {
	if s.guard == nil {
		return
	}
	if err := s.guard.HapusLoginCooldown(ctx); err != nil {
		s.log.Warn("hapus cooldown login portal gagal", "err", err)
	}
}

func (s *Server) handleWAStatus(w http.ResponseWriter, req *http.Request) {
	// Proxy ke GET /api/status whatsapp-service (status koneksi Baileys).
	u := s.waBase + "/api/status"
	ctx, cancel := context.WithTimeout(req.Context(), 10*time.Second)
	defer cancel()
	reqExt, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		s.internalError(w, "siapkan request WA", err)
		return
	}
	reqExt.Header.Set("X-API-Key", s.waAPIKey)
	resp, err := s.waHTTP.Do(reqExt)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "unreachable", "error": err.Error()})
		return
	}
	defer resp.Body.Close()
	var out any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "unreachable", "error": "respons tidak valid"})
		return
	}
	writeJSON(w, resp.StatusCode, out)
}

// handleWAQR proxy GET /api/qr whatsapp-service: image/png saat QR tersedia,
// JSON saat belum. Content-Type dari Layanan WA diteruskan apa adanya.
func (s *Server) handleWAQR(w http.ResponseWriter, req *http.Request) {
	u := s.waBase + "/api/qr"
	ctx, cancel := context.WithTimeout(req.Context(), 15*time.Second)
	defer cancel()
	reqExt, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		s.internalError(w, "siapkan request QR WA", err)
		return
	}
	reqExt.Header.Set("X-API-Key", s.waAPIKey)
	resp, err := s.waHTTP.Do(reqExt)
	if err != nil {
		s.internalError(w, "panggil whatsapp-service QR", err)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// handleWALogout proxy POST /api/logout whatsapp-service: putus sesi Baileys.
func (s *Server) handleWALogout(w http.ResponseWriter, req *http.Request) {
	u := s.waBase + "/api/logout"
	ctx, cancel := context.WithTimeout(req.Context(), 15*time.Second)
	defer cancel()
	reqExt, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
	if err != nil {
		s.internalError(w, "siapkan request logout WA", err)
		return
	}
	reqExt.Header.Set("X-API-Key", s.waAPIKey)
	resp, err := s.waHTTP.Do(reqExt)
	if err != nil {
		s.internalError(w, "panggil whatsapp-service logout", err)
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		s.internalError(w, "baca respons logout WA", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

// SendReportToRecipients mengirim satu laporan ke semua recipient aktif,
// satu pesan per penerima, stagger 2 detik antar kiriman. Hasil dicatat ke
// send_log. Error per-penerima tidak menghentikan penerima berikutnya.
func (s *Server) SendReportToRecipients(ctx context.Context, reportID int64, content string) {
	recipients, err := s.st.ListRecipients(ctx, true)
	if err != nil {
		s.log.Error("baca recipients untuk kirim laporan", "err", err)
		return
	}
	if len(recipients) == 0 {
		s.log.Info("tidak ada recipient aktif, laporan hanya tersimpan", "report_id", reportID)
		return
	}
	for i, r := range recipients {
		if i > 0 {
			// Stagger antar pesan agar tidak diblokir WhatsApp.
			select {
			case <-time.After(2 * time.Second):
			case <-ctx.Done():
				return
			}
		}
		entry := store.SendEntry{ReportID: reportID, RecipientID: r.ID, Phone: r.Phone}
		if err := s.sendText(ctx, r.Phone, content); err != nil {
			entry.Status = "error"
			entry.Error = err.Error()
			s.log.Error("kirim laporan gagal", "phone", r.Phone, "err", err)
		} else {
			entry.Status = "sent"
			s.log.Info("kirim laporan sukses", "phone", r.Phone, "report_id", reportID)
		}
		if err := s.st.LogSend(ctx, entry, time.Now()); err != nil {
			s.log.Warn("gagal catat send_log", "phone", r.Phone, "err", err)
		}
	}
}

// sendText memanggil POST {base}/send-text whatsapp-service dengan body
// {phone, message}. Endpoint ini disesuaikan dari app.js smsgateway-grita.
func (s *Server) sendText(ctx context.Context, phone, message string) error {
	if s.waBase == "" {
		return fmt.Errorf("WA_BASE_URL belum dikonfigurasi")
	}
	payload, err := json.Marshal(map[string]string{"phone": phone, "message": message})
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	reqExt, err := http.NewRequestWithContext(ctx, http.MethodPost, s.waBase+"/send-text", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("siapkan request: %w", err)
	}
	reqExt.Header.Set("Content-Type", "application/json")
	reqExt.Header.Set("X-API-Key", s.waAPIKey)
	resp, err := s.waHTTP.Do(reqExt)
	if err != nil {
		return fmt.Errorf("request send-text: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("send-text status HTTP %d", resp.StatusCode)
	}
	return nil
}

// RenderReport adalah alias tipografi agar caller lintas package konsisten
// menggunakan tipe metrics dari package report.
type RenderReport = report.ServerMetrics // kompilasi: tipe alias dari package report

func (s *Server) decode(w http.ResponseWriter, req *http.Request, v any) bool {
	dec := json.NewDecoder(req.Body)
	if err := dec.Decode(v); err != nil {
		http.Error(w, "body JSON tidak valid", http.StatusBadRequest)
		return false
	}
	return true
}

func (s *Server) internalError(w http.ResponseWriter, msg string, err error) {
	s.log.Error(msg, "err", err)
	http.Error(w, "kesalahan internal server", http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
