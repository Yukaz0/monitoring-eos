// Command collector menjalankan SATU siklus penuh: metrik Prometheus +
// sweep portal + render + simpan DB + kirim WhatsApp, lalu mencetak hasil
// ke stdout dan mengirim POST ke API. Mode tanpa portal: SKIP_PORTAL=1.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/grita/iconics-eos-monitoring/internal/cycle"
	"github.com/grita/iconics-eos-monitoring/internal/store"
)

const (
	defaultAPIBase = "http://localhost:5118"
	defaultSQLDsn  = "file:/data/monitoring.db?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	postTimeout    = 30 * time.Second
)

func main() {
	var (
		apiBase   = flag.String("api", envOr("API_BASE_URL", defaultAPIBase), "base URL monitoring-api")
		apiKey    = flag.String("key", os.Getenv("API_KEY"), "API key internal (header X-API-Key)")
		dsn       = flag.String("dsn", envOr("SQLITE_DSN", defaultSQLDsn), "DSN SQLite")
		noPost    = flag.Bool("no-post", false, "jangan POST hasil ke API")
		skipStore = flag.Bool("stdout-only", false, "cetak laporan saja tanpa DB/kirim WA")
	)
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()

	st, err := store.Open(*dsn)
	if err != nil {
		log.Error("gagal buka sqlite", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	c, err := cycle.New(st, os.Getenv("SCADA_PORTAL_TOKEN"),
		envOr("WA_BASE_URL", "http://whatsapp-service:3101"), os.Getenv("WA_API_KEY"), log)
	if err != nil {
		log.Error("gagal inisialisasi siklus", "err", err)
		os.Exit(1)
	}
	if *skipStore {
		// Mode cepat: hanya render dan cetak, tanpa kirim WA.
		// (SkipPortal tetap mengikuti env SKIP_PORTAL.)
		c.WASender = nil
	}
	content, err := c.Run(ctx)
	if err != nil {
		log.Error("siklus gagal", "err", err)
		os.Exit(1)
	}

	// Hasil ke stdout: inilah laporan final.
	fmt.Println(content)

	if *noPost || *skipStore {
		return
	}
	if err := postReport(ctx, *apiBase, *apiKey, content); err != nil {
		log.Error("gagal POST laporan ke API", "err", err)
		os.Exit(1)
	}
	log.Info("laporan terkirim ke API", "api", *apiBase)
}

// postReport mengirim laporan jadi ke API (disimpan ke report_history
// lewat POST /api/report/submit tanpa memicu siklus baru).
func postReport(ctx context.Context, apiBase, apiKey, content string) error {
	payload, err := json.Marshal(map[string]string{"content": content})
	if err != nil {
		return fmt.Errorf("marshal report: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiBase+"/api/report/submit", bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("siapkan request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	client := &http.Client{Timeout: postTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("status HTTP %d dari API", resp.StatusCode)
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
