package api

import (
	"context"
	"sync"
	"time"
)

// CycleTracker mencatat status siklus laporan. Siklus berjalan lama (60 detik
// bila semua RTU aktif, sampai 10 menit bila ada RTU yang diam) sementara
// POST /api/report/run menjawab 202 seketika, jadi dashboard butuh sumber data
// nyata untuk spinner/progress: GET /api/report/status.
type CycleTracker struct {
	mu      sync.Mutex
	running bool
	started time.Time
	last    time.Duration
}

// Lacak membungkus RunCycler supaya setiap siklus - dipicu tombol dashboard
// maupun jadwal cron - tercatat sedang berjalan dan berapa lama selesainya.
func (t *CycleTracker) Lacak(run RunCycler) RunCycler {
	return func(ctx context.Context) {
		t.mulai()
		defer t.selesai()
		run(ctx)
	}
}

func (t *CycleTracker) mulai() {
	t.mu.Lock()
	t.running = true
	t.started = time.Now()
	t.mu.Unlock()
}

func (t *CycleTracker) selesai() {
	t.mu.Lock()
	if t.running {
		t.last = time.Since(t.started)
	}
	t.running = false
	t.mu.Unlock()
}

// Status mengembalikan apakah siklus sedang berjalan, kapan dimulai, dan
// berapa lama siklus terakhir yang sudah selesai.
func (t *CycleTracker) Status() (running bool, started time.Time, last time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.running, t.started, t.last
}
