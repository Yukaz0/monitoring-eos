// Command api menjalankan REST API monitoring + scheduler cron laporan.
// Siklus laporan penuh (metrik + sweep portal + render + kirim WA) dirakit
// di package cycle dan dipasang ke scheduler serta POST /api/report/run.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/grita/iconics-eos-monitoring/internal/api"
	"github.com/grita/iconics-eos-monitoring/internal/cycle"
	"github.com/grita/iconics-eos-monitoring/internal/scheduler"
	"github.com/grita/iconics-eos-monitoring/internal/store"
)

func main() {
	// Subcommand healthcheck untuk Docker HEALTHCHECK (image tidak punya
	// curl/wget): monitoring-api --healthcheck [host:port].
	if len(os.Args) > 1 && os.Args[1] == "--healthcheck" {
		target := "localhost:5118"
		if len(os.Args) > 2 {
			target = os.Args[2]
		}
		c := &http.Client{Timeout: 4 * time.Second}
		resp, err := c.Get("http://" + target + "/api/health")
		if err != nil || resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		resp.Body.Close()
		fmt.Println("ok")
		os.Exit(0)
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))

	addr := envOr("LISTEN_ADDR", ":5118")
	dsn := envOr("SQLITE_DSN", "file:/data/monitoring.db?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	apiKey := os.Getenv("API_KEY")
	waBase := envOr("WA_BASE_URL", "http://whatsapp-service:3101")
	waAPIKey := os.Getenv("WA_API_KEY")

	st, err := store.Open(dsn)
	if err != nil {
		log.Error("gagal buka sqlite", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	// Siklus laporan: token portal dari env SCADA_PORTAL_TOKEN; bila kosong
	// diambil dari SQLite (diisi via PUT /api/session/token).
	cycleFn, progress, loginPortal, loginGuard := loadCycleHook(st, waBase, waAPIKey, log)
	if cycleFn == nil {
		cycleFn = func(_ context.Context) {
			log.Warn("siklus laporan tidak aktif (periksa konfigurasi)")
		}
	}

	// DISABLE_INTERNAL_SCHEDULER=1 mematikan cron internal container:
	// scheduler dirakit tanpa runner sehingga tidak ada job cron yang
	// didaftarkan (Reload early-return saat run nil) dan Start() tidak
	// dipanggil. Baris tabel schedules di SQLite tidak diubah agar UI tetap
	// menampilkan jadwal. Penjadwalan dijalankan dari host lewat systemd user
	// timer (scripts/run_report_host.py), sedangkan POST /api/report/run
	// tetap memakai cycleFn.
	schedDisabled := os.Getenv("DISABLE_INTERNAL_SCHEDULER") == "1"
	// Tracker mencatat siklus berjalan untuk GET /api/report/status; dibungkus
	// sekali di sini supaya siklus manual maupun jadwal cron sama-sama tercatat.
	tracker := &api.CycleTracker{}
	cycleTracked := tracker.Lacak(cycleFn)
	var sched *scheduler.Scheduler
	if schedDisabled {
		sched = scheduler.New(st, log, nil)
	} else {
		sched = scheduler.New(st, log, scheduler.Runner(cycleTracked))
	}
	srv := api.NewServer(api.Config{
		Store:       st,
		APIKey:      apiKey,
		WABase:      waBase,
		WAAPIKey:    waAPIKey,
		Sched:       sched,
		Cycle:       cycleTracked,
		Tracker:     tracker,
		Progress:    progress,
		LoginPortal: loginPortal,
		LoginGuard:  loginGuard,
		Log:         log,
	})

	if schedDisabled {
		log.Info("scheduler internal dinonaktifkan (DISABLE_INTERNAL_SCHEDULER=1); jadwal dijalankan dari host")
	} else if err := sched.Start(context.Background()); err != nil {
		log.Warn("sebagian jadwal gagal dimuat", "err", err)
	}

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Info("monitoring-api berjalan", "addr", addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server berhenti", "err", err)
			os.Exit(1)
		}
	}()

	// Graceful shutdown.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Info("shutdown...")
	ctxShut, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctxShut)
	sched.Stop()
}

// loadCycleHook merakit siklus laporan penuh lewat package cycle
// (prometheus + portal + report + store + pengirim WA). Nilai kembalian nil
// berarti siklus tidak aktif. Provider progres dipakai GET /api/report/status
// untuk progress bar dashboard (berapa RTU sudah mengirim dari yang ditunggu).
// LoginPortal dipakai POST /api/session/login; nil bila kredensial kosong.
// LoginGuard menghubungkan endpoint login manual ke cooldown persisten tanpa
// membuat package api mengimpor internal/cycle.
func loadCycleHook(st *store.Store, waBase, waAPIKey string, log *slog.Logger) (api.RunCycler, api.ProgressProvider, api.LoginPortal, api.LoginGuard) {
	token := os.Getenv("SCADA_PORTAL_TOKEN")
	if token == "" {
		if tok, ok := cycle.PortalTokenFromStore(context.Background(), st); ok {
			token = tok
		}
	}
	c, err := cycle.New(st, token, waBase, waAPIKey, log)
	if err != nil {
		log.Warn("siklus laporan tidak aktif", "err", err)
		return nil, nil, nil, nil
	}
	guard := loginGuardAdapter{st: st, menit: int(c.LoginCooldown / time.Minute)}
	return cycle.RunCycleFn(c), c, c.LoginPortalSiap(), guard
}

// loginGuardAdapter memenuhi api.LoginGuard dari *store.Store (penyimpanan
// cooldown) + *cycle.Cycle (durasi dari env). Dipisah di sini supaya package
// api tetap tidak bergantung pada internal/cycle.
type loginGuardAdapter struct {
	st    *store.Store
	menit int
}

func (g loginGuardAdapter) CatatLoginGagal(ctx context.Context, sampai time.Time, pesan string) error {
	return g.st.CatatLoginGagal(ctx, sampai, pesan)
}

func (g loginGuardAdapter) HapusLoginCooldown(ctx context.Context) error {
	return g.st.HapusLoginCooldown(ctx)
}

func (g loginGuardAdapter) CooldownMenit() int { return g.menit }

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
