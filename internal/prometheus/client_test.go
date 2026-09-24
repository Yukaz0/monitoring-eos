package prometheus

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// promJSON membentuk respons /api/v1/query untuk satu nilai.
func promJSON(value string) string {
	return `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1758000000,"` + value + `"]}]}}`
}

func TestQueryNilaiSederhana(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			t.Errorf("path = %q, want /api/v1/query", r.URL.Path)
		}
		if got := r.URL.Query().Get("query"); got != "node_boot_time_seconds" {
			t.Errorf("query = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(promJSON("1758000000")))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	v, err := c.Query(ctx, "node_boot_time_seconds")
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if v != 1758000000 {
		t.Fatalf("nilai = %v, want 1758000000", v)
	}
}

func TestQueryErrorRespons(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
	}{
		{"http 500", http.StatusInternalServerError, "boom"},
		{"status error", http.StatusOK, `{"status":"error","errorType":"bad_data"}`},
		{"hasil kosong", http.StatusOK, `{"status":"success","data":{"resultType":"vector","result":[]}}`},
		{"nilai bukan angka", http.StatusOK, promJSON("abc")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := NewClient(srv.URL)
			_, err := c.Query(context.Background(), "up")
			if err == nil {
				t.Fatalf("harus error untuk kasus %q", tc.name)
			}
		})
	}
}

func TestQueryTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		_, _ = w.Write([]byte(promJSON("1")))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	if _, err := c.Query(ctx, "up"); err == nil {
		t.Fatal("harus timeout")
	}
}

func TestFetchServerMetrics(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(q, "node_time_seconds"):
			_, _ = w.Write([]byte(promJSON("10675200"))) // /86400 = 123.556 hari
		case strings.Contains(q, "MemAvailable"):
			_, _ = w.Write([]byte(promJSON("54.32")))
		case strings.Contains(q, "SwapTotal"):
			_, _ = w.Write([]byte(promJSON("1.5")))
		case strings.Contains(q, "MemTotal"):
			_, _ = w.Write([]byte(promJSON("99857989558"))) // ~93 GiB
		case strings.Contains(q, `mode="idle"`):
			_, _ = w.Write([]byte(promJSON("12.3456"))) // hasil akhir 100*(1-avg idle)
		case strings.Contains(q, "node_load1"):
			_, _ = w.Write([]byte(promJSON("5.44")))
		case strings.Contains(q, "container_last_seen"):
			_, _ = w.Write([]byte(promJSON("21")))
		case strings.HasPrefix(q, "count(count"):
			_, _ = w.Write([]byte(promJSON("64")))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"status":"error"}`))
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	m, err := c.FetchServerMetrics(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	if m.UptimeDays < 123.5 || m.UptimeDays > 123.6 {
		t.Errorf("uptime = %v, want ~123.556", m.UptimeDays)
	}
	if m.CPUCores != 64 {
		t.Errorf("cores = %v, want 64", m.CPUCores)
	}
	if m.CPUBusy < 12.3 || m.CPUBusy > 12.4 {
		t.Errorf("cpu busy = %v, want ~12.35", m.CPUBusy)
	}
	// Sys load = 5.44 * 100 / 64 = 8.5
	if m.SysLoad < 8.49 || m.SysLoad > 8.51 {
		t.Errorf("sys load = %v, want ~8.5", m.SysLoad)
	}
	if m.RAMUsed < 54.3 || m.RAMUsed > 54.4 {
		t.Errorf("ram used = %v", m.RAMUsed)
	}
	if m.SwapUsed < 1.4 || m.SwapUsed > 1.6 {
		t.Errorf("swap used = %v, want ~1.5", m.SwapUsed)
	}
	if m.Containers != 21 {
		t.Errorf("containers = %v, want 21", m.Containers)
	}
}

func TestFetchServerMetricsSwapAbsent(t *testing.T) {
	// Node tanpa swap: semua query sukses kecuali SwapTotal (404).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(q, "SwapTotal") {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"status":"error"}`))
			return
		}
		_, _ = w.Write([]byte(promJSON("1")))
	}))
	defer srv.Close()

	c := NewClient(srv.URL)
	m, err := c.FetchServerMetrics(context.Background())
	if err != nil {
		t.Fatalf("fetch harus tetap sukses saat swap absent: %v", err)
	}
	if m.SwapUsed != 0 {
		t.Errorf("swap = %v, want 0", m.SwapUsed)
	}
}
