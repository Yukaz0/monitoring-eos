// Package portal berisi scraper chromedp untuk status telemetri RTU
// dari portal https://scadatr.grita.id (halaman MQTT Client Subscriber).
//
// TODO(portal-in-container): sweep portal via chromium headful di Xvfb
// masih flaky (net::ERR_CONNECTION_RESET intermiten pada navigasi overview
// walaupun curl dari container OK). Sementara sweep portal dijalankan
// dari host (skrip python scripts/run_report_host.py), hasil dikirim via
// POST /api/report/submit. Aktifkan kembali bila jaringan container
// stabil atau pindah host.
//
// Quy tắc keandalan: chromium di container bisa mati di tengah operasi
// (OOM, crash zygote). Saat itu terjadi chromedp.Run bisa balik error,
// dan Poll bisa menggantung bila goroutine read-loop websocket sudah
// mati; semua langkah harus diberi subcontext timeout dan dipantau log.
package portal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

// ErrSessionExpired dikembalikan bila portal me-redirect ke /login
// (token kedaluwarsa, umur JWT ~24 jam).
var ErrSessionExpired = errors.New("sesi portal kedaluwarsa, token baru diperlukan")

// Konstanta portal.
const (
	portalOrigin    = "https://scadatr.grita.id"
	pageMQTT        = "/system/configure/front-end-processor/mqtt/client"
	pageOverview    = "/overview"
	localStorageKey = "persist:grita-scada-tr"

	// Portal merender tabel via socket.io dalam sesi headful: tabel bisa
	// kosong beberapa puluh detik. Beri timeout besar, jangan dipangkas.
	navTimeout   = 240 * time.Second
	tableTimeout = 240 * time.Second
	detailWait   = 6 * time.Second  // tunggu setelah klik anchor RTU
	emptyRetry   = 20 * time.Second // tunggu ulang untuk tabel kosong

	pollAuthTimeout = 50 * time.Second // Poll isAuthenticated di verifyAuth
	maxRTURetries   = 2                // usaha ulang per RTU bila chromium mati
)

// StationStatus adalah hasil sweep satu RTU/lambung.
type StationStatus struct {
	RTUID         string
	Station       string
	BrokerState   string
	TelemetryRows int
}

// Collector menjalankan sweep portal dengan chromedp headful (Xvfb).
type Collector struct {
	token      string // raw JSON localStorage value dari SCADA_PORTAL_TOKEN
	execChrome string // path chromium, kosong = deteksi otomatis
	log        *slog.Logger
}

// NewCollector membuat collector; token adalah string JSON mentah hasil
// scripts/extract_token.py yang di-inject ke localStorage sebelum
// navigasi ke halaman aplikasi mana pun.
func NewCollector(token, execChrome string, log *slog.Logger) *Collector {
	if log == nil {
		log = slog.Default()
	}
	return &Collector{token: token, execChrome: execChrome, log: log}
}

// allocOpts merakit opsi ExecAllocator (headful + no-sandbox di container).
func (c *Collector) allocOpts() []chromedp.ExecAllocatorOption {
	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		// WAJIB headful: headless terbukti merender portal shell dengan
		// tabel kosong (docs/arsitektur.md). Jalankan di bawah Xvfb.
		chromedp.Flag("headless", false),
		// WAJIB di container: binary jalan sebagai root, Chromium menolak
		// sandbox kernel tanpa flag ini (crash instan, zygite host).
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.Flag("start-maximized", true),
	)
	if c.execChrome != "" {
		opts = append(opts, chromedp.ExecPath(c.execChrome))
	}
	return opts
}

// browser set menampung konteks chromedp aktif beserta fungsi bersih yang
// bisa diganti saat chromium restart di tengah sweep.
type browserSet struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// newBrowser membuat allocator + konteks browser chromedp segar dengan
// deadline keseluruhan 30 menit dan memaksa chromium diluncurkan supaya
// error peluncuran terlihat cepat. Pemanggil wajib memanggil cancel.
func (c *Collector) newBrowser(ctx context.Context, boot bool) (browserSet, error) {
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(ctx, c.allocOpts()...)
	bctx, cancelCtx := chromedp.NewContext(allocCtx, chromedp.WithLogf(func(format string, a ...any) {
		c.log.Info("cdp", "msg", fmt.Sprintf(format, a...))
	}))
	// Deadline keseluruhan sweep: 21 RTU x (6s klik + tabel) + navigasi.
	bctx, cancelDead := context.WithTimeout(bctx, 30*time.Minute)
	cancel := func() {
		cancelDead()
		cancelCtx()
		cancelAlloc()
	}
	if boot {
		// Paksa browser hidup sekarang: error launch muncul di sini.
		if err := chromedp.Run(bctx); err != nil {
			cancel()
			return browserSet{}, fmt.Errorf("portal: gagal luncurkan chromium: %w", err)
		}
	}
	return browserSet{ctx: bctx, cancel: cancel}, nil
}

// runStep membungkus blok chromedp.Run dengan subcontext timeout dan log
// sebelum/sesudah; kunci supaya operasi apa pun tidak menggantung tanpa
// batas ketika chromium tewas di tengah jalan.
func (c *Collector) runStep(ctx context.Context, name string, timeout time.Duration, actions ...chromedp.Action) error {
	c.log.Info(name + " dimulai")
	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := chromedp.Run(sctx, actions...)
	if err != nil {
		c.log.Error(name+" gagal", "err", err)
		return fmt.Errorf("portal: %s: %w", name, err)
	}
	c.log.Info(name + " selesai")
	return nil
}

// Collect menjalankan satu sweep penuh: buka browser headful, inject token,
// verifikasi auth, baca tabel subscriber, klik tiap RTU, kumpulkan status.
func (c *Collector) Collect(ctx context.Context) ([]StationStatus, error) {
	if c.token == "" {
		return nil, fmt.Errorf("portal: SCADA_PORTAL_TOKEN kosong")
	}

	b, err := c.newBrowser(ctx, true)
	if err != nil {
		return nil, err
	}
	defer b.cancel()

	if err := c.injectToken(b.ctx); err != nil {
		return nil, err
	}
	if err := c.verifyAuth(b.ctx); err != nil {
		return nil, err
	}
	if err := c.openSubscriberTable(b.ctx); err != nil {
		return nil, err
	}
	rows, err := c.readSubscriberRows(b.ctx)
	if err != nil {
		return nil, err
	}
	c.log.Info("tabel subscriber terbaca", "rtu", len(rows))
	return c.sweepStations(&b, rows)
}

// injectToken membuka /login (halaman ringan pada origin portal),
// menyetel localStorage key persist:grita-scada-tr dengan token mentah,
// lalu me-reload agar app SPA membaca token saat boot.
func (c *Collector) injectToken(ctx context.Context) error {
	c.log.Info("token portal di-inject ke localStorage", "origin", portalOrigin, "len", len(c.token))
	if err := c.runStep(ctx, "injectToken buka /login", navTimeout,
		chromedp.Navigate(portalOrigin+"/login")); err != nil {
		return err
	}

	var stored string
	if err := c.runStep(ctx, "injectToken set localStorage + reload", navTimeout,
		chromedp.Evaluate(
			fmt.Sprintf(`localStorage.setItem(%[1]q, %[2]q); localStorage.getItem(%[1]q)`,
				localStorageKey, c.token),
			&stored,
		),
		chromedp.Reload(),
	); err != nil {
		return fmt.Errorf("portal: gagal set localStorage: %w", err)
	}
	if stored == "" {
		return fmt.Errorf("portal: localStorage %q tidak tersimpan", localStorageKey)
	}
	c.log.Info("injectToken selesai")
	return nil
}

// verifyAuth: buka /overview, poll isAuthenticated paling lama 50 detik.
// Fallback: jika URL tidak mengandung /login, sesi dianggap sukses
// (cek lokasi adalah sinyal paling andal, semua halaman private
// me-redirect ke /login saat token mati).
func (c *Collector) verifyAuth(ctx context.Context) error {
	c.log.Info("verifyAuth dimulai")

	var curURL string
	// Jangan cek /login dini: SPA bisa berakhir di /login sesaat sebelum
	// rehydrate token dari localStorage. Poll 50 detik adalah judge utama.
	if err := c.runStep(ctx, "verifyAuth navigasi /overview", navTimeout,
		chromedp.Navigate(portalOrigin+pageOverview),
		chromedp.WaitReady("body"),
		chromedp.Sleep(3*time.Second),
	); err != nil {
		return err
	}

	authJS := `(function () {
  var raw = localStorage.getItem("persist:grita-scada-tr");
  if (!raw) return false;
  try {
    var root = JSON.parse(raw);
    if (!root.auth) return false;
    var auth = JSON.parse(root.auth);
    return auth.isAuthenticated === true;
  } catch (e) { return false; }
})()`

	c.log.Info("verifyAuth poll isAuthenticated dimulai", "timeout", pollAuthTimeout)
	pctx, pcancel := context.WithTimeout(ctx, pollAuthTimeout+15*time.Second)
	defer pcancel()
	err := chromedp.Run(pctx,
		chromedp.Poll(authJS, nil,
			chromedp.WithPollingTimeout(pollAuthTimeout),
			chromedp.WithPollingInterval(500*time.Millisecond)),
	)
	if err == nil {
		c.log.Info("verifyAuth selesai: isAuthenticated true")
		return nil
	}
	c.log.Warn("verifyAuth poll isAuthenticated belum true, fallback cek lokasi", "err", err)

	// Fallback: anggap sukses bila URL tidak mengandung /login.
	fctx, fcancel := context.WithTimeout(ctx, 30*time.Second)
	defer fcancel()
	if lerr := chromedp.Run(fctx, chromedp.Location(&curURL)); lerr != nil {
		return fmt.Errorf("portal: verifyAuth fallback baca lokasi: %w", lerr)
	}
	if strings.Contains(curURL, "/login") {
		return ErrSessionExpired
	}
	c.log.Warn("verifyAuth fallback: sesi dianggap sukses berdasarkan lokasi", "url", curURL)
	return nil
}

// openSubscriberTable membuka halaman MQTT client dan menunggu tabel
// subscriber punya baris (paling lambat tableTimeout).
func (c *Collector) openSubscriberTable(ctx context.Context) error {
	return c.runStep(ctx, "openSubscriberTable", navTimeout,
		chromedp.Navigate(portalOrigin+pageMQTT),
		chromedp.WaitReady("body"),
		chromedp.Poll(`document.querySelectorAll("table tr td").length > 8`, nil,
			chromedp.WithPollingTimeout(tableTimeout-10*time.Second),
			chromedp.WithPollingInterval(1*time.Second)),
	)
}

// subscriberRow adalah satu baris tabel subscriber yang sudah diparsing.
type subscriberRow struct {
	RTUID   string
	Station string
	Status  string
}

// rowsJS membaca semua baris tabel subscriber: table tr dengan td.length
// > 8; tiap baris dipetakan ke array innerText selnya.
const rowsJS = `Array.from(document.querySelectorAll("table tr"))
  .filter(function (tr) { return tr.querySelectorAll("td").length > 8; })
  .map(function (tr) {
    return Array.from(tr.querySelectorAll("td")).map(function (td) {
      return td.innerText.trim();
    });
  })`

// readSubscriberRows memParsing tabel subscriber. Tabel via socket.io bisa
// kosong beberapa puluh detik: tunggu ulang sekali dengan timeout penuh.
func (c *Collector) readSubscriberRows(ctx context.Context) ([]subscriberRow, error) {
	var raw [][]string
	if err := c.runStep(ctx, "readSubscriberRows", tableTimeout,
		chromedp.Evaluate(rowsJS, &raw),
	); err != nil {
		return nil, fmt.Errorf("portal: gagal baca tabel subscriber: %w", err)
	}
	if len(raw) == 0 {
		c.log.Warn("tabel subscriber kosong, tunggu ulang", "tambahan", emptyRetry)
		rctx, cancel2 := context.WithTimeout(ctx, tableTimeout)
		defer cancel2()
		err := chromedp.Run(rctx,
			chromedp.Poll(`document.querySelectorAll("table tr td").length > 8`, nil,
				chromedp.WithPollingTimeout(emptyRetry),
				chromedp.WithPollingInterval(1*time.Second)),
			chromedp.Evaluate(rowsJS, &raw),
		)
		if err != nil {
			return nil, fmt.Errorf("portal: tabel subscriber tetap kosong: %w", err)
		}
	}

	rows := make([]subscriberRow, 0, len(raw))
	for _, cells := range raw {
		if len(cells) <= 7 {
			continue
		}
		rows = append(rows, subscriberRow{RTUID: cells[1], Station: cells[2], Status: cells[7]})
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("portal: tidak ada baris subscriber valid")
	}
	return rows, nil
}

// bootstrapSteps menyiapkan ulang browser yang baru direstart: inject
// token, verify auth, openSubscriberTable. Dipakai awal sweep dan restart.
func (c *Collector) bootstrapSteps(ctx context.Context) error {
	if err := c.injectToken(ctx); err != nil {
		return err
	}
	if err := c.verifyAuth(ctx); err != nil {
		return err
	}
	if err := c.openSubscriberTable(ctx); err != nil {
		return err
	}
	// Setelah restart tidak perlu baca ulang tabel: daftar rows lama
	// tetap berlaku (anchor di pohon kiri cukup stabil).
	return nil
}

// sweepStations klik tiap RTU di tree kiri dan baca detail broker+telemetri.
// Progres tiap RTU dilog; bila chromium mati di tengah, konteks browser
// dibuat ulang dan RTU itu diulang, paling banyak maxRTURetries kali.
func (c *Collector) sweepStations(b *browserSet, rows []subscriberRow) ([]StationStatus, error) {
	total := len(rows)
	out := make([]StationStatus, 0, total)
	for i, row := range rows {
		if err := b.ctx.Err(); err != nil {
			return out, fmt.Errorf("portal: sweep dibatalkan di RTU ke-%d: %w", i+1, err)
		}
		c.log.Info("sweep progres RTU", "urutan", fmt.Sprintf("%d dari %d", i+1, total),
			"rtu", row.RTUID, "station", row.Station)

		var st *StationStatus
		var lastErr error
		for attempt := 0; attempt <= maxRTURetries; attempt++ {
			if attempt > 0 {
				c.log.Warn("chromium kemungkinan mati di tengah RTU, restart konteks browser",
					"rtu", row.RTUID, "percobaan", attempt, "err", lastErr)
				nb, nerr := c.newBrowser(b.ctx, false)
				if nerr != nil {
					lastErr = fmt.Errorf("portal: restart konteks browser: %w", nerr)
					continue
				}
				if berr := c.bootstrapSteps(nb.ctx); berr != nil {
					nb.cancel()
					lastErr = fmt.Errorf("portal: bootstrap ulang browser: %w", berr)
					continue
				}
				*b = nb
			}
			st, lastErr = c.sweepOne(b.ctx, row)
			if lastErr == nil {
				break
			}
		}
		if st == nil {
			c.log.Error("RTU gagal setelah batas percobaan, lanjut RTU berikutnya",
				"rtu", row.RTUID, "err", lastErr)
			continue
		}
		out = append(out, *st)
		c.log.Info("RTU selesai", "urutan", fmt.Sprintf("%d dari %d", i+1, total),
			"rtu", row.RTUID, "station", row.Station,
			"broker", st.BrokerState, "rows", st.TelemetryRows)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("portal: tidak ada RTU yang berhasil discan")
	}
	return out, nil
}

// anchorCoordsJS mencari anchor <a> yang innerText-nya diawali RTU ID,
// lalu mengembalikan koordinat klik: mid-x tapi min 20px dari left, mid-y.
func anchorCoordsJS(rtuID string) string {
	return fmt.Sprintf(`(function () {
  var links = Array.from(document.querySelectorAll("a"));
  var a = links.find(function (el) {
    return (el.innerText || "").trim().startsWith(%q);
  });
  if (!a) return null;
  var r = a.getBoundingClientRect();
  return {x: Math.max(r.left + r.width / 2, r.left + 20), y: r.top + r.height / 2};
})()`, rtuID)
}

// sweepOne klik satu RTU via koordinat, tunggu detail termuat (detailWait),
// baca Broker Status dari body innerText dan hitung baris tabel telemetri.
// Tabel kosong diulang sekali dengan tunggu emptyRetry.
func (c *Collector) sweepOne(ctx context.Context, row subscriberRow) (*StationStatus, error) {
	var coords struct {
		X float64 `json:"x"`
		Y float64 `json:"y"`
	}
	if err := c.runStep(ctx, "cari anchor RTU "+row.RTUID, tableTimeout,
		chromedp.Evaluate(anchorCoordsJS(row.RTUID), &coords),
	); err != nil {
		return nil, fmt.Errorf("portal: cari anchor RTU %s: %w", row.RTUID, err)
	}
	if coords.X == 0 && coords.Y == 0 {
		return nil, fmt.Errorf("portal: anchor RTU %s tidak ditemukan di tree kiri", row.RTUID)
	}

	// Klik via koordinat lalu tunggu panel detail: tabel butuh waktu lama
	// terisi via socket.io di sesi headful, jadi tunggu dengan longgar.
	var bodyText string
	dctx, cancel2 := context.WithTimeout(ctx, tableTimeout)
	defer cancel2()
	err := chromedp.Run(dctx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			return chromedp.MouseClickXY(coords.X, coords.Y).Do(ctx)
		}),
		chromedp.Sleep(detailWait),
		chromedp.ActionFunc(func(ctx context.Context) error {
			return chromedp.Evaluate(`document.body.innerText`, &bodyText).Do(ctx)
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("portal: klik RTU %s: %w", row.RTUID, err)
	}

	brokerState := extractBrokerState(bodyText)
	if brokerState == "" {
		return nil, fmt.Errorf("portal: Broker Status tidak ditemukan untuk RTU %s", row.RTUID)
	}

	rowsCount := c.countTelemetryRows(ctx, row.RTUID)
	return &StationStatus{
		RTUID:       row.RTUID,
		Station:     row.Station,
		BrokerState: brokerState,
		// Baris telemetri > 0 berarti lambung active (broker connected
		// dan data mengalir); 0 berarti no data from broker.
		TelemetryRows: rowsCount,
	}, nil
}

// brokerStatusRE menangkap "Broker Status - <state>" dari innerText body.
var brokerStatusRE = regexp.MustCompile(`Broker Status\s*-\s*([A-Za-z ]+)`)

func extractBrokerState(bodyText string) string {
	m := brokerStatusRE.FindStringSubmatch(bodyText)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

// telemetryRowsJS menghitung baris tabel yang header-nya mengandung "Topic"
// (tabel telemetri Topic/Value/Type/Quality/Timetag), minus baris header.
const telemetryRowsJS = `(function () {
  var tables = Array.from(document.querySelectorAll("table"));
  var t = tables.find(function (tb) { return (tb.innerText || "").indexOf("Topic") !== -1; });
  if (!t) return 0;
  return t.querySelectorAll("tr").length - 1;
})()`

// countTelemetryRows menghitung baris tabel telemetri; bila kosong, ulang
// sekali setelah emptyRetry karena data via socket.io bisa datang belakangan.
func (c *Collector) countTelemetryRows(ctx context.Context, rtuID string) int {
	count := func(ctx context.Context) (int, error) {
		var n int
		err := chromedp.Evaluate(telemetryRowsJS, &n).Do(ctx)
		return n, err
	}

	count = func(ctx context.Context) (int, error) {
		var n int
		sctx, scancel := context.WithTimeout(ctx, 30*time.Second)
		defer scancel()
		err := chromedp.Evaluate(telemetryRowsJS, &n).Do(sctx)
		return n, err
	}
	n, err := count(ctx)
	if err != nil {
		c.log.Warn("gagal hitung tabel telemetri", "rtu", rtuID, "err", err)
		return 0
	}
	if n > 0 {
		return n
	}
	// Ulangi sekali dengan tunggu lebih panjang untuk tabel kosong.
	rctx, cancel := context.WithTimeout(ctx, emptyRetry+10*time.Second)
	defer cancel()
	select {
	case <-rctx.Done():
		return 0
	case <-time.After(emptyRetry):
	}
	n, err = count(rctx)
	if err != nil {
		c.log.Warn("gagal hitung ulang tabel telemetri", "rtu", rtuID, "err", err)
		return 0
	}
	return n
}
