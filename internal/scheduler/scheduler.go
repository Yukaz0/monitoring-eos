// Package scheduler menjalankan jadwal cron laporan dengan zona Asia/Jakarta.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/grita/iconics-eos-monitoring/internal/store"
	"github.com/robfig/cron/v3"
)

// Runner dipanggil scheduler setiap kali jadwal terpicu (siklus laporan penuh).
type Runner func(ctx context.Context)

// Scheduler membungkus robfig/cron v3 dengan lokasi Asia/Jakarta dan sinkron
// jadwal dari tabel schedules SQLite. Reload aman dipanggil dari handler PUT.
type Scheduler struct {
	st   *store.Store
	log  *slog.Logger
	run  Runner
	cron *cron.Cron

	mu    sync.Mutex
	ids   map[cron.EntryID]store.Schedule
	start bool
}

// New membuat scheduler. Runner boleh nil (untuk pengujian).
func New(st *store.Store, log *slog.Logger, run Runner) *Scheduler {
	if log == nil {
		log = slog.Default()
	}
	loc, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		// time/tzdata di-import di package report; fallback zona tetap WIB.
		loc = time.FixedZone("WIB", 7*3600)
	}
	return &Scheduler{
		st:   st,
		log:  log,
		run:  run,
		ids:  map[cron.EntryID]store.Schedule{},
		cron: cron.New(cron.WithLocation(loc), cron.WithChain(cron.Recover(cron.PrintfLogger(logWriter{log})))),
	}
}

// logWriter mengadaptasi *slog.Logger ke cron.PrintfLogger (interface Printf).
type logWriter struct{ log *slog.Logger }

func (w logWriter) Printf(format string, args ...interface{}) {
	w.log.Info(fmt.Sprintf(format, args...))
}

// Start memuat jadwal dari DB lalu menjalankan engine cron.
func (s *Scheduler) Start(ctx context.Context) error {
	if err := s.Reload(ctx); err != nil {
		return err
	}
	s.cron.Start()
	s.mu.Lock()
	s.start = true
	s.mu.Unlock()
	s.log.Info("scheduler cron berjalan", "zona", s.cron.Location().String())
	return nil
}

// Stop menghentikan engine cron dan menunggu job berjalan selesai.
func (s *Scheduler) Stop() context.Context {
	return s.cron.Stop()
}

// Reload membuang semua entry lama lalu mendaftarkan ulang jadwal aktif
// dari DB. Aman dipanggil kapan pun (dipakai handler PUT /api/schedules).
func (s *Scheduler) Reload(ctx context.Context) error {
	if s.run == nil {
		// Scheduler dinonaktifkan (mis. DISABLE_INTERNAL_SCHEDULER=1):
		// jadwal tetap tersimpan di DB, tapi tidak ada job cron yang
		// didaftarkan. Bukan error, handler PUT tetap balas 200.
		s.log.Warn("scheduler internal nonaktif; perubahan jadwal hanya disimpan, tidak dijalankan")
		return nil
	}
	scheds, err := s.st.ListSchedules(ctx, true)
	if err != nil {
		return fmt.Errorf("scheduler: baca schedules: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.ids {
		s.cron.Remove(id)
	}
	s.ids = map[cron.EntryID]store.Schedule{}

	var firstErr error
	for _, sc := range scheds {
		entry, err := s.cron.AddFunc(sc.CronExpr, func() {
			jobCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			if err := s.st.MarkScheduleRun(jobCtx, sc.ID, time.Now()); err != nil {
				s.log.Warn("gagal catat last_run_at", "schedule", sc.Name, "err", err)
			}
			s.log.Info("jadwal terpicu", "schedule", sc.Name, "cron", sc.CronExpr)
			s.run(jobCtx)
		})
		if err != nil {
			// Ekspresi cron di DB rusak: catat dan lanjut ke jadwal lain,
			// jangan sampai satu baris rusak mematikan seluruh scheduler.
			s.log.Error("ekspresi cron tidak valid", "schedule", sc.Name, "cron", sc.CronExpr, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		s.ids[entry] = sc
		s.log.Info("jadwal terdaftar", "schedule", sc.Name, "cron", sc.CronExpr)
	}
	return firstErr
}
