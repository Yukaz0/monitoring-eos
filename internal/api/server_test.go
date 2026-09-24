package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/grita/iconics-eos-monitoring/internal/store"
)

func newTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open("file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("buka sqlite: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	srv := NewServer(Config{
		Store:    st,
		APIKey:   "kunci-uji",
		WABase:   "http://whatsapp-invalid.test",
		WAAPIKey: "wa-kunci",
	})
	return srv, st
}

func doJSON(t *testing.T, h http.Handler, method, path, apiKey, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	req = req.WithContext(context.Background())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHealthTanpaAPIKey(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := doJSON(t, srv.Handler(), http.MethodGet, "/api/health", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("health = %d, want 200", rec.Code)
	}
}

func TestMutasiWajibAPIKey(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()

	// Tanpa API key: 401.
	if rec := doJSON(t, h, http.MethodPost, "/api/recipients", "", `{"phone":"6281"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST tanpa key = %d, want 401", rec.Code)
	}
	// API key salah: 401.
	if rec := doJSON(t, h, http.MethodPost, "/api/recipients", "salah", `{"phone":"6281"}`); rec.Code != http.StatusUnauthorized {
		t.Fatalf("POST key salah = %d, want 401", rec.Code)
	}
	// Endpoint lain yang butuh key juga harus menolak.
	for _, tc := range []struct{ method, path string }{
		{http.MethodPut, "/api/schedules"},
		{http.MethodPost, "/api/report/run"},
		{http.MethodPut, "/api/session/token"},
	} {
		if rec := doJSON(t, h, tc.method, tc.path, "", `{}`); rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s tanpa key = %d, want 401", tc.method, tc.path, rec.Code)
		}
	}
}

func TestRecipientEndpoints(t *testing.T) {
	srv, st := newTestServer(t)
	h := srv.Handler()

	// Create.
	rec := doJSON(t, h, http.MethodPost, "/api/recipients", "kunci-uji",
		`{"name":"Operator A","phone":"628111111111"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d, want 201: %s", rec.Code, rec.Body.String())
	}
	var created store.Recipient
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.ID == 0 || created.Phone != "628111111111" || !created.Active {
		t.Fatalf("created salah: %+v", created)
	}

	// Create tanpa phone: 400.
	if rec := doJSON(t, h, http.MethodPost, "/api/recipients", "kunci-uji", `{"name":"x"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("create tanpa phone = %d, want 400", rec.Code)
	}

	// List.
	rec = doJSON(t, h, http.MethodGet, "/api/recipients", "", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "628111111111") {
		t.Fatalf("list = %d %s", rec.Code, rec.Body.String())
	}

	// Update.
	rec = doJSON(t, h, http.MethodPut, "/api/recipients/1", "kunci-uji", `{"name":"A2"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "A2") {
		t.Fatalf("update = %d %s", rec.Code, rec.Body.String())
	}

	// Update id tidak ada: 404.
	rec = doJSON(t, h, http.MethodPut, "/api/recipients/99", "kunci-uji", `{"name":"x"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("update missing = %d, want 404", rec.Code)
	}

	// Delete.
	rec = doJSON(t, h, http.MethodDelete, "/api/recipients/1", "kunci-uji", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete = %d", rec.Code)
	}
	rec = doJSON(t, h, http.MethodDelete, "/api/recipients/1", "kunci-uji", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete dua kali = %d, want 404", rec.Code)
	}

	_ = st
}

func TestScheduleEndpointReloadScheduler(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()

	rec := doJSON(t, h, http.MethodPut, "/api/schedules", "kunci-uji",
		`{"name":"uji","cron_expr":"40 7 * * *","active":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("put schedule = %d %s", rec.Code, rec.Body.String())
	}
	var sc store.Schedule
	if err := json.Unmarshal(rec.Body.Bytes(), &sc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if sc.CronExpr != "40 7 * * *" {
		t.Fatalf("schedule salah: %+v", sc)
	}

	// Cron tidak valid: tetap tersimpan tapi handler tetap 200 (scheduler
	// mencatat error dan melewati entry itu).
	rec = doJSON(t, h, http.MethodPut, "/api/schedules", "kunci-uji",
		`{"name":"rusak","cron_expr":"bukan-cron","active":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("put schedule rusak = %d %s", rec.Code, rec.Body.String())
	}

	// List.
	rec = doJSON(t, h, http.MethodGet, "/api/schedules", "", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "uji") {
		t.Fatalf("list schedules = %d %s", rec.Code, rec.Body.String())
	}
}

func TestReportEndpoints(t *testing.T) {
	srv, st := newTestServer(t)
	h := srv.Handler()

	// Latest saat kosong: 404.
	if rec := doJSON(t, h, http.MethodGet, "/api/report/latest", "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("latest kosong = %d, want 404", rec.Code)
	}

	if _, err := st.SaveReport(context.Background(), time.Now(), "isi laporan"); err != nil {
		t.Fatalf("save: %v", err)
	}

	rec := doJSON(t, h, http.MethodGet, "/api/report/latest", "", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "isi laporan") {
		t.Fatalf("latest = %d %s", rec.Code, rec.Body.String())
	}

	rec = doJSON(t, h, http.MethodGet, "/api/report/history?limit=5", "", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "isi laporan") {
		t.Fatalf("history = %d %s", rec.Code, rec.Body.String())
	}

	// Submit laporan jadi (dipakai collector standalone).
	rec = doJSON(t, h, http.MethodPost, "/api/report/submit", "kunci-uji",
		`{"content":"laporan dari collector"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("submit = %d %s", rec.Code, rec.Body.String())
	}

	// Run tanpa cycler terpasang: 503.
	rec = doJSON(t, h, http.MethodPost, "/api/report/run", "kunci-uji", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("run tanpa cycler = %d, want 503", rec.Code)
	}
}

// fakeProgress meniru *cycle.Cycle untuk menguji field progres di
// GET /api/report/status.
type fakeProgress struct {
	collected, expected int
	aktif               bool
}

func (f fakeProgress) Progress() (int, int, bool) {
	return f.collected, f.expected, f.aktif
}

func TestReportStatusTracker(t *testing.T) {
	tr := &CycleTracker{}
	mulai := make(chan struct{})
	lepas := make(chan struct{})
	selesaiJalan := make(chan struct{})
	run := tr.Lacak(func(context.Context) {
		close(mulai)
		<-lepas
	})
	go func() {
		run(context.Background())
		close(selesaiJalan)
	}()
	<-mulai

	if running, _, _ := tr.Status(); !running {
		t.Error("saat siklus berjalan: running harus true")
	}

	srv, st := newTestServer(t)
	srv.tracker = tr
	srv.progress = fakeProgress{collected: 14, expected: 22, aktif: true}
	if _, err := st.SaveReport(context.Background(), time.Now(), "laporan uji"); err != nil {
		t.Fatalf("save: %v", err)
	}
	rec := doJSON(t, srv.Handler(), http.MethodGet, "/api/report/status", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"running":true`) {
		t.Errorf("respons harus running:true, dapat %s", body)
	}
	if !strings.Contains(body, `"report_id"`) || !strings.Contains(body, `"perkiraan_sisa_detik"`) {
		t.Errorf("respons kurang report_id/perkiraan_sisa_detik: %s", body)
	}
	// Progres pengumpulan: 14 dari 22 RTU -> fase mengumpulkan, belum 100%.
	if !strings.Contains(body, `"rtu_terkumpul":14`) || !strings.Contains(body, `"rtu_diharapkan":22`) {
		t.Errorf("respons kurang rtu_terkumpul/rtu_diharapkan: %s", body)
	}
	if !strings.Contains(body, `"fase":"mengumpulkan_traffic"`) {
		t.Errorf("fase harus mengumpulkan_traffic: %s", body)
	}
	if !strings.Contains(body, `"persen":63`) {
		t.Errorf("persen harus 63: %s", body)
	}

	close(lepas)
	<-selesaiJalan
	running, _, last := tr.Status()
	if running {
		t.Error("setelah selesai: running harus false")
	}
	if last <= 0 {
		t.Errorf("durasi siklus terakhir harus > 0, dapat %v", last)
	}
	rec = doJSON(t, srv.Handler(), http.MethodGet, "/api/report/status", "", "")
	if !strings.Contains(rec.Body.String(), `"running":false`) {
		t.Errorf("respons harus running:false, dapat %s", rec.Body.String())
	}
}

func TestSessionEndpoints(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()

	// Status awal.
	rec := doJSON(t, h, http.MethodGet, "/api/session/status", "", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status awal = %d", rec.Code)
	}

	// Set token.
	rec = doJSON(t, h, http.MethodPut, "/api/session/token", "kunci-uji",
		`{"token":"{\"auth\":\"x\"}"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("set token = %d %s", rec.Code, rec.Body.String())
	}

	// Token kosong ditolak.
	rec = doJSON(t, h, http.MethodPut, "/api/session/token", "kunci-uji", `{"token":""}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("token kosong = %d, want 400", rec.Code)
	}

	// Status setelah set: valid false (belum diverifikasi collector).
	rec = doJSON(t, h, http.MethodGet, "/api/session/status", "", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "\"valid\":false") {
		t.Fatalf("status setelah set = %d %s", rec.Code, rec.Body.String())
	}
}

func TestWAStatusUnreachable(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := doJSON(t, srv.Handler(), http.MethodGet, "/api/wa/status", "", "")
	// Host tidak ada: tetap 200 dengan status unreachable (bukan 500),
	// agar frontend bisa menampilkan status tanpa meledak.
	if rec.Code != http.StatusOK {
		t.Fatalf("wa status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unreachable") && !strings.Contains(rec.Body.String(), "status") {
		t.Fatalf("wa status body = %s", rec.Body.String())
	}
}
