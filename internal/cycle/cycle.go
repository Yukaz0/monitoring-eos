// Package cycle merakit siklus laporan penuh: metrik Prometheus + sweep
// portal + render + simpan DB + kirim WhatsApp. Dipakai cmd/api (hook)
// dan cmd/collector (mode CLI satu siklus).
package cycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/grita/iconics-eos-monitoring/internal/api"
	"github.com/grita/iconics-eos-monitoring/internal/portal"
	"github.com/grita/iconics-eos-monitoring/internal/portalclient"
	"github.com/grita/iconics-eos-monitoring/internal/prometheus"
	"github.com/grita/iconics-eos-monitoring/internal/report"
	"github.com/grita/iconics-eos-monitoring/internal/store"
)

const (
	defaultPromBase = "http://192.168.3.204:9090"
	sendTimeout     = 2 * time.Minute

	// Mode pembaca portal (env PORTAL_MODE). Mode browser memakai scraper
	// chromedp seperti sebelumnya; mode socket memakai pembaca headless
	// portalclient (socket.io polling + REST) tanpa Chromium sama sekali.
	PortalModeBrowser = "browser"
	PortalModeSocket  = "socket"

	// defaultPortalWindowSeconds sama dengan default
	// PORTAL_LOG_WINDOW_SECONDS: jendela pengumpulan traffic maksimum.
	defaultPortalWindowSeconds = 600

	// defaultPortalMinWindowSeconds sama dengan default
	// PORTAL_LOG_MIN_WINDOW_SECONDS: jendela pengumpulan traffic minimum
	// sebelum sweep boleh berhenti lebih awal.
	defaultPortalMinWindowSeconds = 60

	// defaultPortalLoginCooldownMinutes sama dengan default
	// PORTAL_LOGIN_COOLDOWN_MINUTES: lama berhenti mencoba login otomatis
	// setelah satu kegagalan, supaya akun portal tidak diblokir.
	defaultPortalLoginCooldownMinutes = 360

	// maxRingkasLoginErr membatasi panjang ringkasan kegagalan login yang
	// disimpan ke store (bukan pesan penuh).
	maxRingkasLoginErr = 200

	portalBrowserTimeout = 30 * time.Minute // satu sweep chromedp penuh
	portalSocketTimeout  = 45 * time.Second // tunggu event mqtt / REST cadangan
)

// ReportSender mengirim laporan jadi ke recipient aktif. Dipenuhi oleh
// *api.Server; dibuat sebagai interface supaya keputusan menahan pengiriman
// bisa diuji tanpa menyentuh WhatsApp.
type ReportSender interface {
	SendReportToRecipients(ctx context.Context, reportID int64, content string)
}

// PembacaPortalSocket adalah bagian portalclient.Reader yang dipakai siklus.
// Dibuat sebagai interface supaya sweep (dan login otomatis) bisa diuji
// dengan pembaca tiruan tanpa jaringan.
type PembacaPortalSocket interface {
	FetchClients(ctx context.Context, token string) ([]portalclient.Client, error)
	CollectLogs(ctx context.Context, token string, opt portalclient.CollectOptions) (map[int]portalclient.LogStat, error)
}

// InfoSumberPortal dilaporkan pembaca yang tahu dari mana daftar klien terakhir
// datang (event socket vs cadangan REST). Dipakai gate pengiriman: pembacaan
// lewat cadangan REST tidak memuat status broker sehingga setiap RTU akan
// tampak "no data from broker", dan laporan seperti itu memang harus ditahan.
// Pembaca tiruan tidak wajib memenuhinya.
type InfoSumberPortal interface {
	SumberKlienTerakhir() string
}

// Cycle menyimpan dependensi satu siklus laporan.
type Cycle struct {
	Store      *store.Store
	Prom       *prometheus.Client
	Portal     *portal.Collector   // terisi hanya pada mode browser
	Socket     PembacaPortalSocket // terisi hanya pada mode socket
	WASender   ReportSender        // hanya untuk SendReportToRecipients
	Log        *slog.Logger
	SkipPortal bool // mode tanpa portal (uji lokal / metrik saja)

	// socketReader adalah pembaca portalclient asli (bukan interface), dipakai
	// untuk login REST; nil pada mode browser.
	socketReader *portalclient.Reader

	// SendWithoutTelemetry membolehkan pengiriman laporan walau bagian
	// telemetri RTU gagal dibaca (env SEND_WITHOUT_TELEMETRY=1). Default
	// false: laporan setengah jadi ditahan dan hanya disimpan.
	SendWithoutTelemetry bool

	// PortalMode menentukan pembaca portal: PortalModeBrowser (default) atau
	// PortalModeSocket.
	PortalMode string
	// PortalToken adalah isi SCADA_PORTAL_TOKEN / tabel portal_session apa
	// adanya (JSON localStorage, atau JWT mentah).
	PortalToken string
	// PortalUser, PortalPass, dan PortalTOTPSecret adalah kredensial login
	// REST portal (env PORTAL_USERNAME/PORTAL_PASSWORD/PORTAL_TOTP_SECRET).
	// Nilainya tidak pernah dicetak ke log.
	PortalUser       string
	PortalPass       string
	PortalTOTPSecret string
	// PortalLoginAktif menandakan login otomatis portal diaktifkan
	// (env PORTAL_LOGIN_ENABLED=1).
	PortalLoginAktif bool
	// LoginPortal menjalankan login siap pakai dan mengembalikan token. Diisi
	// di New() dari socketReader; nil berarti login dilewati.
	LoginPortal func(ctx context.Context) (string, error)
	// LoginCooldown adalah lama berhenti mencoba login otomatis setelah satu
	// kegagalan (env PORTAL_LOGIN_COOLDOWN_MINUTES, default 360 menit).
	LoginCooldown time.Duration
	// PortalWindow adalah batas lama pengumpulan event traffic pada mode
	// socket (PORTAL_LOG_WINDOW_SECONDS).
	PortalWindow time.Duration
	// PortalMinWindow adalah lama pengumpulan minimum pada mode socket
	// (PORTAL_LOG_MIN_WINDOW_SECONDS). Setelah jendela ini lewat, sweep
	// berhenti lebih awal bila semua RTU Connected sudah mengirim minimal
	// satu event traffic.
	PortalMinWindow time.Duration

	// Progres pengumpulan traffic pada siklus yang sedang berjalan, dibaca
	// HTTP handler lain lewat Progress(). Atomik karena diisi dari goroutine
	// pengumpul sementara dashboard membacanya dari goroutine request.
	progressCollected atomic.Int64
	progressExpected  atomic.Int64
	progressAktif     atomic.Bool
}

// catatProgress dipanggil pengumpul log setiap putaran polling (lewat
// CollectOptions.OnProgress) untuk memperbarui progres yang dibaca dashboard.
func (c *Cycle) catatProgress(collected, expected int) {
	c.progressCollected.Store(int64(collected))
	c.progressExpected.Store(int64(expected))
}

// Progress melaporkan kemajuan pengumpulan traffic RTU: berapa yang sudah
// mengirim, berapa yang ditunggu, dan apakah fase pengumpulan masih aktif.
// Dipakai GET /api/report/status untuk progress bar dashboard.
func (c *Cycle) Progress() (collected, expected int, aktif bool) {
	return int(c.progressCollected.Load()), int(c.progressExpected.Load()), c.progressAktif.Load()
}

// Run menjalankan satu siklus penuh dan mengembalikan isi laporan.
func (c *Cycle) Run(ctx context.Context) (string, error) {
	c.Log.Info("siklus dimulai")

	// 1. Metrik server dari Prometheus.
	sctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	metrics, err := c.Prom.FetchServerMetrics(sctx)
	if err != nil {
		c.Log.Error("siklus gagal: metrik prometheus tidak terbaca", "err", err)
		return "", fmt.Errorf("cycle: metrik prometheus: %w", err)
	}

	// 2. Sweep portal (status telemetri RTU).
	var stations []report.Station
	telemetryOK := false
	if !c.SkipPortal {
		st, err := c.collectPortal(ctx)
		switch {
		case errorsIs(err, portal.ErrSessionExpired):
			// Tandai sesi mati; laporan tetap dirender dengan bagian
			// telemetri kosong, dan operator diberi tahu via log.
			_ = c.Store.MarkPortalSession(ctx, false)
			c.Log.Error("sesi portal kedaluwarsa; perbarui token via PUT /api/session/token",
				"err", err)
		case err != nil:
			// Tanpa log ini siklus bisa berakhir tanpa jejak apa pun, dan
			// penyebabnya tidak bisa dibaca dari mana pun.
			c.Log.Error("siklus gagal: sweep portal", "err", err)
			return "", fmt.Errorf("cycle: sweep portal: %w", err)
		default:
			_ = c.Store.MarkPortalSession(ctx, true)
			stations = st
			telemetryOK = true
			if c.PortalMode == PortalModeSocket {
				telemetryOK = c.telemetriSah()
			}
		}
	}

	// 3. Render laporan.
	srv := report.ServerMetrics{
		UptimeDays:    metrics.UptimeDays,
		RAMTotalBytes: metrics.RAMTotalBytes,
		CPUCores:      metrics.CPUCores,
		CPUBusy:       metrics.CPUBusy,
		SysLoad:       metrics.SysLoad,
		RAMUsed:       metrics.RAMUsed,
		SwapUsed:      metrics.SwapUsed,
		Containers:    int(metrics.Containers),
	}
	content := report.Render(time.Now(), srv, stations)

	// 4. Simpan ke riwayat.
	reportID, err := c.Store.SaveReport(ctx, time.Now(), content)
	if err != nil {
		c.Log.Error("siklus gagal: simpan laporan", "err", err)
		return "", fmt.Errorf("cycle: simpan laporan: %w", err)
	}
	c.Log.Info("laporan tersimpan", "report_id", reportID)

	// 5. Kirim ke semua recipient aktif (stagger di dalam sender), kecuali
	// bagian telemetri RTU gagal dibaca: laporan setengah jadi ditahan dan
	// hanya disimpan, sesuai keputusan operator.
	c.kirimLaporan(ctx, telemetryOK, reportID, content)

	return content, nil
}

// kirimLaporan mengirim laporan ke recipient hanya bila diizinkan, dan
// mengembalikan true bila benar-benar dikirim.
func (c *Cycle) kirimLaporan(ctx context.Context, telemetryOK bool, reportID int64, content string) bool {
	boleh, alasan := c.pengirimanDiizinkan(telemetryOK)
	if !boleh {
		c.logger().Warn("pengiriman laporan ditahan: telemetri RTU belum lengkap",
			"report_id", reportID, "sebab", alasan)
		return false
	}
	if alasan != "" {
		c.logger().Warn("laporan dikirim tanpa bagian telemetri RTU",
			"report_id", reportID, "sebab", alasan)
	}
	sctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	c.WASender.SendReportToRecipients(sctx, reportID, content)
	return true
}

// logger mengembalikan logger siklus, atau default bila belum di-set.
func (c *Cycle) logger() *slog.Logger {
	if c.Log == nil {
		return slog.Default()
	}
	return c.Log
}

// pengirimanDiizinkan memutuskan apakah laporan boleh dikirim ke recipient.
// Telemetri RTU wajib berhasil dibaca; operator hanya bisa melonggarkan
// sementara lewat SEND_WITHOUT_TELEMETRY=1 (mis. saat sesi portal harus
// diperbarui manual). Alasan non-kosong dipakai untuk log.
func (c *Cycle) pengirimanDiizinkan(telemetryOK bool) (bool, string) {
	if telemetryOK {
		return true, ""
	}
	if c.SendWithoutTelemetry {
		return true, "SEND_WITHOUT_TELEMETRY=1"
	}
	return false, "belum semua RTU yang diharapkan mengirim traffic"
}

// errorsIs adalah pembungkus errors.Is untuk keterbacaan siklus.
func errorsIs(err, target error) bool {
	return errors.Is(err, target)
}

// collectPortal menjalankan sweep portal sesuai PortalMode: pembaca headless
// portalclient (PortalModeSocket) atau scraper chromedp (PortalModeBrowser).
func (c *Cycle) collectPortal(ctx context.Context) ([]report.Station, error) {
	if c.PortalMode == PortalModeSocket {
		return c.collectPortalSocket(ctx)
	}
	if c.Portal == nil {
		return nil, errors.New("cycle: collector portal browser belum dirakit")
	}
	pctx, pcancel := context.WithTimeout(ctx, portalBrowserTimeout)
	defer pcancel()
	statuses, err := c.Portal.Collect(pctx)
	if err != nil {
		return nil, err
	}
	out := make([]report.Station, 0, len(statuses))
	for _, st := range statuses {
		out = append(out, report.Station{Station: st.Station, TelemetryRows: st.TelemetryRows})
	}
	return out, nil
}

// collectPortalSocket membaca status telemetri lewat socket.io + REST portal
// (tanpa Chromium) lalu mengklasifikasi sesuai aturan operator. Pengumpulan
// traffic memakai jendela adaptif: minimum c.PortalMinWindow, berhenti lebih
// awal bila semua RTU Connected sudah mengirim, maksimum c.PortalWindow.
// Timestamp traffic terakhir tetap dikumpulkan untuk log dan progres, tetapi
// tidak dirender ke laporan (format laporan kembali seperti biasa).
func (c *Cycle) collectPortalSocket(ctx context.Context) ([]report.Station, error) {
	if c.Socket == nil {
		return nil, errors.New("cycle: pembaca portal socket belum dirakit")
	}
	// loginDicoba membatasi login otomatis maksimum sekali per siklus;
	// loginDilakukan hanya menandai keberhasilan untuk log akhir sweep.
	// Token terbaru dari store dipakai lebih dulu: jalur lain (endpoint login,
	// PUT /api/session/token, push_token.py) bisa sudah menulis token segar
	// sementara token di memori masih yang lama.
	c.segarkanTokenDariStore(ctx)
	loginDicoba := false
	loginDilakukan := false
	if c.perluLogin() && c.LoginPortal != nil {
		loginDicoba = true
		loginDilakukan = c.loginSekarang(ctx)
	}

	fctx, fcancel := context.WithTimeout(ctx, portalSocketTimeout)
	defer fcancel()
	fetchStart := time.Now()
	var clients []portalclient.Client
	err := c.cobaDenganLogin(fctx, &loginDicoba, func() error {
		var e error
		clients, e = c.Socket.FetchClients(fctx, c.PortalToken)
		return e
	})
	if err != nil {
		return nil, wrapPortalAuth(err)
	}
	fetchElapsed := time.Since(fetchStart)

	lctx, lcancel := context.WithTimeout(ctx, c.PortalWindow+portalSocketTimeout)
	defer lcancel()
	logStart := time.Now()
	expected := connectedRTUs(clients)
	// Progres pengumpulan (untuk progress bar dashboard): fase ini yang
	// menentukan lama siklus, jadi statusnya harus terlihat.
	c.progressAktif.Store(true)
	c.progressExpected.Store(int64(len(expected)))
	c.progressCollected.Store(0)
	defer c.progressAktif.Store(false)
	var logs map[int]portalclient.LogStat
	err = c.cobaDenganLogin(lctx, &loginDicoba, func() error {
		var e error
		logs, e = c.Socket.CollectLogs(lctx, c.PortalToken, portalclient.CollectOptions{
			MinWindow:  c.PortalMinWindow,
			MaxWindow:  c.PortalWindow,
			Expected:   expected,
			OnProgress: c.catatProgress,
		})
		return e
	})
	if err != nil {
		return nil, wrapPortalAuth(err)
	}
	logElapsed := time.Since(logStart)

	stations := classifySocket(clients, logs)
	active := 0
	for i, cl := range clients {
		if stations[i].TelemetryRows == 0 {
			continue
		}
		active++
		// Nilai mentah dipertahankan: panel/event memakai UTC, sebagian device
		// mengirim waktu lokal berlabel Z (lihat docs/arsitektur.md).
		c.Log.Info("traffic terakhir portal", "rtu", cl.RTUID,
			"station", stations[i].Station,
			"timestamp", logs[cl.RTUID].LastTraffic,
			"event", logs[cl.RTUID].Events)
	}
	c.Log.Info("sweep portal socket selesai", "klien", len(clients), "active", active,
		"jendela_min_detik", int(c.PortalMinWindow.Seconds()),
		"jendela_maks_detik", int(c.PortalWindow.Seconds()),
		"fetch_detik", fetchElapsed.Round(time.Millisecond).String(),
		"collect_detik", logElapsed.Round(time.Millisecond).String(),
		"berhenti_lebih_awal", logElapsed < c.PortalWindow,
		"login_dilakukan", loginDilakukan)
	return stations, nil
}

// segarkanTokenDariStore memakai token yang tersimpan di SQLite bila exp-nya
// lebih baru daripada token di memori. Token di store bisa ditulis jalur lain
// (PUT /api/session/token, POST /api/session/login, scripts/push_token.py) dan
// siklus harus memakai yang paling baru: tanpa ini siklus memakai token mati
// di memori, melaporkan "sesi kedaluwarsa", dan menahan laporan padahal token
// baru sudah tersimpan.
func (c *Cycle) segarkanTokenDariStore(ctx context.Context) {
	if c.Store == nil {
		return
	}
	ps, err := c.Store.GetPortalSession(ctx)
	if err != nil || strings.TrimSpace(ps.Token) == "" {
		return
	}
	expBaru, okBaru := portalclient.TokenExpiry(ps.Token)
	expLama, okLama := portalclient.TokenExpiry(c.PortalToken)
	if !okBaru || (okLama && !expBaru.After(expLama)) {
		return
	}
	c.PortalToken = ps.Token
	c.logger().Info("token portal disegarkan dari store",
		"exp", expBaru.Local().Format("2006-01-02 15:04 MST"))
}

// perluLogin melaporkan apakah token portal perlu disegarkan: token kosong,
// atau exp-nya sudah lewat / kurang dari dua menit lagi (login butuh waktu).
func (c *Cycle) perluLogin() bool {
	if strings.TrimSpace(c.PortalToken) == "" {
		return true
	}
	exp, ok := portalclient.TokenExpiry(c.PortalToken)
	if !ok {
		return false
	}
	return time.Until(exp) < 2*time.Minute
}

// cobaDenganLogin menjalankan op lalu, bila op ditolak portal
// (ErrUnauthorized) dan login belum dicoba pada siklus ini, login sekali dan
// ulangi op sekali lagi. Maksimum satu login dan satu percobaan ulang per
// siklus; kegagalan login tidak menutupi error asli.
func (c *Cycle) cobaDenganLogin(ctx context.Context, loginDicoba *bool, op func() error) error {
	err := op()
	if !errors.Is(err, portalclient.ErrUnauthorized) || *loginDicoba || c.LoginPortal == nil {
		return err
	}
	*loginDicoba = true
	if !c.loginSekarang(ctx) {
		return err
	}
	return op()
}

// loginSekarang menjalankan login otomatis portal, menyimpan token baru
// dengan valid=true, dan memperbarui c.PortalToken. Cooldown kegagalan
// dihormati lebih dulu supaya satu kesalahan kredensial tidak menghabiskan
// jatah percobaan dan memblokir akun operator. Kegagalan hanya dicatat sebagai
// WARN (siklus tetap berjalan dengan token lama), jadi selalu mengembalikan
// bool alih-alih error. Token tidak pernah dicetak, hanya exp.
func (c *Cycle) loginSekarang(ctx context.Context) bool {
	if c.LoginPortal == nil {
		return false
	}
	if c.Store != nil {
		sampai, pesan, ok, err := c.Store.LoginCooldown(ctx, time.Now())
		if err != nil {
			c.logger().Warn("baca cooldown login portal gagal", "err", err)
		} else if ok {
			lokal := sampai.Local().Format("2006-01-02 15:04 MST")
			c.logger().Warn("login portal otomatis dilewati: cooldown sampai "+lokal,
				"sampai", lokal, "pesan", pesan)
			return false
		}
	}
	token, err := c.LoginPortal(ctx)
	if err != nil || token == "" {
		// "User is already logged in" bukan kesalahan kredensial: portal
		// hanya mengizinkan satu sesi per akun, jadi mengulang login hanya
		// akan gagal lagi sampai sesi lama berakhir. Jangan pasang cooldown
		// (itu untuk kredensial salah) — cukup beri tahu operator.
		if errors.Is(err, portalclient.ErrSudahLogin) {
			c.logger().Warn("login portal otomatis dilewati: akun masih punya sesi aktif di portal; "+
				"tunggu sesi itu berakhir atau logout lebih dulu", "err", err)
			return false
		}
		c.catatLoginGagal(ctx, err)
		return false
	}
	if c.Store != nil {
		if err := c.Store.HapusLoginCooldown(ctx); err != nil {
			c.logger().Warn("hapus cooldown login portal gagal", "err", err)
		}
		if err := c.Store.SavePortalToken(ctx, token, true); err != nil {
			c.logger().Warn("simpan token hasil login portal gagal", "err", err)
			return false
		}
	}
	c.PortalToken = token
	attrs := []any{}
	if exp, ok := portalclient.TokenExpiry(token); ok {
		attrs = append(attrs, "exp_local", exp.Local().Format("2006-01-02 15:04 MST"))
	}
	c.logger().Info("login portal otomatis berhasil", attrs...)
	return true
}

// catatLoginGagal menyimpan cooldown kegagalan login ke store (bila ada) dan
// mencatat WARN. Pesan diringkas tanpa kredensial supaya aman disimpan/dilog.
func (c *Cycle) catatLoginGagal(ctx context.Context, err error) {
	sampai := time.Now().Add(c.LoginCooldown)
	if c.Store != nil {
		if e := c.Store.CatatLoginGagal(ctx, sampai, c.ringkasLoginErr(err)); e != nil {
			c.logger().Warn("simpan cooldown login portal gagal", "err", e)
		}
	}
	c.logger().Warn("login portal otomatis gagal; percobaan berikutnya ditunda",
		"sampai", sampai.Local().Format("2006-01-02 15:04 MST"), "err", err)
}

// ringkasLoginErr memendekkan pesan kegagalan login untuk disimpan sebagai
// cooldown. Password (bila diketahui) dibuang lebih dulu supaya tidak pernah
// tersimpan walau server memantulkannya.
func (c *Cycle) ringkasLoginErr(err error) string {
	if err == nil {
		return ""
	}
	pesan := err.Error()
	if c.PortalPass != "" {
		pesan = strings.ReplaceAll(pesan, c.PortalPass, "[disensor]")
	}
	if runes := []rune(pesan); len(runes) > maxRingkasLoginErr {
		pesan = string(runes[:maxRingkasLoginErr])
	}
	return pesan
}

// telemetriSah memutuskan apakah pembacaan portal dianggap sah untuk pengiriman
// WhatsApp pada mode socket.
//
// Aturan (keputusan operator 2026-09-24): pembacaan sah begitu daftar klien
// datang dari event socket, yang memuat status broker. Jumlah RTU yang mengirim
// TIDAK dipakai sebagai gate — RTU yang memang diam harus muncul di bucket
// "no data from broker" pada laporan, dan satu RTU kronis diam tidak boleh
// membuat laporan tidak pernah terkirim. Yang tetap ditahan hanya pembacaan
// yang jatuh ke cadangan REST: daftar itu tidak memuat status broker sehingga
// setiap RTU akan tampak "no data".
//
// Terpisah dari Run supaya aturannya bisa diuji tanpa memanggil Prometheus.
func (c *Cycle) telemetriSah() bool {
	if c.PortalMode != PortalModeSocket {
		return true
	}
	if info, ok := c.Socket.(InfoSumberPortal); ok {
		return info.SumberKlienTerakhir() != portalclient.SumberREST
	}
	// Pembaca yang tidak melaporkan sumber (mis. tiruan di tes) dianggap sah.
	return true
}

// connectedRTUs mengumpulkan id_rtu yang broker-nya Connected. Hanya RTU ini
// yang mungkin mengirim, jadi hanya mereka yang ditunggu untuk berhenti lebih
// awal; RTU dengan broker mati tetap ikut terdaftar lewat FetchClients dan
// masuk no data from broker tanpa menahan jendela.
func connectedRTUs(clients []portalclient.Client) []int {
	out := make([]int, 0, len(clients))
	for _, cl := range clients {
		if cl.StatusMQTT && cl.RTUID != 0 {
			out = append(out, cl.RTUID)
		}
	}
	return out
}

// classifySocket menerapkan aturan klasifikasi operator: Active = status_mqtt
// true DAN minimal satu event traffic (log-mqtt/datapoint-mqtt) untuk id_rtu
// itu di dalam jendela; selebihnya no data from broker. TelemetryRows dipakai
// sebagai penanda Active (1) / no data (0) supaya template resmi di package
// report tidak perlu berubah. LastTraffic hanya diisi untuk stasiun Active,
// nilainya apa adanya dari panel (UTC).
func classifySocket(clients []portalclient.Client, logs map[int]portalclient.LogStat) []report.Station {
	out := make([]report.Station, 0, len(clients))
	for _, cl := range clients {
		name := cl.Station.Name
		if name == "" {
			name = cl.Name
		}
		rows := 0
		last := ""
		if cl.StatusMQTT && logs[cl.RTUID].Events > 0 {
			rows = 1
			last = logs[cl.RTUID].LastTraffic
		}
		out = append(out, report.Station{Station: name, TelemetryRows: rows, LastTraffic: last})
	}
	return out
}

// wrapPortalAuth menerjemahkan penolakan token dari portalclient menjadi
// portal.ErrSessionExpired supaya penanganan sesi mati tetap sama.
func wrapPortalAuth(err error) error {
	if errors.Is(err, portalclient.ErrUnauthorized) {
		return fmt.Errorf("portal: %w (%v)", portal.ErrSessionExpired, err)
	}
	return err
}

// New merakit Cycle dari environment (dipakai cmd/api dan cmd/collector).
// portalToken adalah string JSON mentah untuk localStorage portal (atau JWT
// mentah pada mode socket); boleh kosong (collector akan error jelas saat
// sweep dijalankan). PORTAL_MODE memilih pembaca portal: "browser" (chromedp,
// default) atau "socket" (headless, tanpa Chromium).
func New(st *store.Store, portalToken, waBase, waAPIKey string, log *slog.Logger) (*Cycle, error) {
	if log == nil {
		log = slog.Default()
	}
	sender := api.NewServer(api.Config{
		Store:    st,
		WABase:   waBase,
		WAAPIKey: waAPIKey,
		Log:      log,
	})
	c := &Cycle{
		Store:           st,
		Prom:            prometheus.NewClient(envOr("PROMETHEUS_URL", defaultPromBase)),
		WASender:        sender,
		Log:             log,
		SkipPortal:      envOr("SKIP_PORTAL", "0") == "1",
		PortalMode:      envOr("PORTAL_MODE", PortalModeBrowser),
		PortalToken:     portalToken,
		PortalWindow:    portalWindow(),
		PortalMinWindow: portalMinWindow(),

		// Kredensial login REST portal; kosong berarti login otomatis mati.
		PortalUser:       os.Getenv("PORTAL_USERNAME"),
		PortalPass:       os.Getenv("PORTAL_PASSWORD"),
		PortalTOTPSecret: os.Getenv("PORTAL_TOTP_SECRET"),
		PortalLoginAktif: envOr("PORTAL_LOGIN_ENABLED", "0") == "1",
		LoginCooldown:    portalLoginCooldown(),

		// Default 0: laporan tanpa telemetri ditahan. Hanya dinyalakan
		// sementara saat operator sadar sesi portal sedang diperbarui.
		SendWithoutTelemetry: envOr("SEND_WITHOUT_TELEMETRY", "0") == "1",
	}
	switch c.PortalMode {
	case PortalModeSocket:
		// Mode headless: chromedp tidak dirakit sama sekali sehingga Chromium
		// tidak mungkin diluncurkan. Port/host default dipegang portalclient
		// (PORTAL_HOST, PORTAL_MQTT_PORT, PORTAL_SYSTEM_PORT).
		sr := portalclient.New(portalclient.Config{
			Host:       envOr("PORTAL_HOST", ""),
			MQTTPort:   envIntOr("PORTAL_MQTT_PORT", 0),
			SystemPort: envIntOr("PORTAL_SYSTEM_PORT", 0),
			Log:        log,
		})
		c.Socket = sr
		c.socketReader = sr
		if c.PortalLoginAktif {
			// Portal tidak punya endpoint refresh: token baru hanya terbit
			// lewat login ulang. Manual (PORTAL_LOGIN_ENABLED=0) tetap mungkin
			// lewat PUT /api/session/token.
			c.LoginPortal = func(ctx context.Context) (string, error) {
				hasil, err := sr.LoginWith2FA(ctx, c.PortalUser, c.PortalPass, c.PortalTOTPSecret)
				if err != nil {
					return "", err
				}
				if hasil.Token == "" {
					return "", errors.New("cycle: login portal tidak mengembalikan token")
				}
				return hasil.Token, nil
			}
		}
	default:
		if c.PortalMode != PortalModeBrowser {
			log.Warn("PORTAL_MODE tidak dikenal, memakai mode browser", "portal_mode", c.PortalMode)
			c.PortalMode = PortalModeBrowser
		}
		c.Portal = portal.NewCollector(portalToken, envOr("CHROME_EXEC", ""), log)
	}
	return c, nil
}

// portalWindow membaca PORTAL_LOG_WINDOW_SECONDS (detik): batas lama
// pengumpulan traffic pada mode socket.
func portalWindow() time.Duration {
	return time.Duration(envIntOr("PORTAL_LOG_WINDOW_SECONDS", defaultPortalWindowSeconds)) * time.Second
}

// portalMinWindow membaca PORTAL_LOG_MIN_WINDOW_SECONDS (detik): lama
// pengumpulan minimum sebelum sweep boleh berhenti lebih awal pada mode
// socket.
func portalMinWindow() time.Duration {
	return time.Duration(envIntOr("PORTAL_LOG_MIN_WINDOW_SECONDS", defaultPortalMinWindowSeconds)) * time.Second
}

// portalLoginCooldown membaca PORTAL_LOGIN_COOLDOWN_MINUTES (menit): lama
// berhenti mencoba login otomatis setelah kegagalan. Nilai kosong/<=0 dianggap
// default.
func portalLoginCooldown() time.Duration {
	return time.Duration(envIntOr("PORTAL_LOGIN_COOLDOWN_MINUTES", defaultPortalLoginCooldownMinutes)) * time.Minute
}

// LoginPortalSiap mengembalikan fungsi login untuk endpoint
// POST /api/session/login, atau nil bila kredensial login portal belum
// dikonfigurasi. Memakai pembaca socket yang sama dengan siklus supaya
// konfigurasi host/port tidak terduplikasi di cmd/api.
func (c *Cycle) LoginPortalSiap() api.LoginPortal {
	if c == nil || c.socketReader == nil {
		return nil
	}
	if strings.TrimSpace(c.PortalUser) == "" || strings.TrimSpace(c.PortalPass) == "" {
		return nil
	}
	return func(ctx context.Context) (api.HasilLoginPortal, error) {
		hasil, err := c.socketReader.LoginWith2FA(ctx, c.PortalUser, c.PortalPass, c.PortalTOTPSecret)
		if err != nil {
			return api.HasilLoginPortal{}, err
		}
		out := api.HasilLoginPortal{Token: hasil.Token, DuaFaktor: hasil.DuaFaktor}
		if exp, ok := portalclient.TokenExpiry(hasil.Token); ok {
			out.Exp = exp
		}
		// Token di memori ikut diperbarui supaya siklus yang sedang berjalan
		// (atau langsung setelahnya) memakai token yang baru saja diterbitkan.
		c.PortalToken = hasil.Token
		return out, nil
	}
}

// RunCycleFn adalah fungsi siklus siap pakai untuk api.RunCycler.
func RunCycleFn(c *Cycle) api.RunCycler {
	return func(ctx context.Context) {
		if _, err := c.Run(ctx); err != nil {
			// Error sudah dilog di dalam Run; runner hanya perlu tahu selesai.
			_ = err
		}
	}
}

// envOr mengembalikan nilai environment variable atau default.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// envIntOr mengembalikan nilai integer environment variable atau default;
// nilai kosong/bukan angka/tidak positif dianggap tidak di-set.
func envIntOr(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n <= 0 {
		return def
	}
	return n
}
