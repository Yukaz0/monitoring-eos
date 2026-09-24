// Package prometheus menyediakan client HTTP sederhana untuk mengambil
// metrik server SCADA (rocky-server) dari endpoint /api/v1/query Prometheus.
package prometheus

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const defaultTimeout = 30 * time.Second

// Client adalah wrapper HTTP ke Prometheus.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient membuat client baru. Contoh baseURL: http://192.168.3.204:9090
func NewClient(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		http:    &http.Client{Timeout: defaultTimeout},
	}
}

// queryResult adalah struktur respons /api/v1/query Prometheus.
type queryResult struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string          `json:"resultType"`
		Result     json.RawMessage `json:"result"`
	} `json:"data"`
}

// Query menjalankan satu query PromQL dan mengembalikan nilai sampel pertama.
// Query count()/scalar() mengembalikan satu nilai sehingga cukup diambil
// elemen pertama dari result vector.
func (c *Client) Query(ctx context.Context, query string) (float64, error) {
	if c.baseURL == "" {
		return 0, fmt.Errorf("prometheus: baseURL belum diatur")
	}
	u, err := url.Parse(c.baseURL + "/api/v1/query")
	if err != nil {
		return 0, fmt.Errorf("prometheus: baseURL tidak valid: %w", err)
	}
	q := u.Query()
	q.Set("query", query)
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, fmt.Errorf("prometheus: gagal membuat request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("prometheus: request gagal: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("prometheus: status HTTP tidak terduga %d", resp.StatusCode)
	}

	var qr queryResult
	if err := json.NewDecoder(resp.Body).Decode(&qr); err != nil {
		return 0, fmt.Errorf("prometheus: gagal decode respons: %w", err)
	}
	if qr.Status != "success" {
		return 0, fmt.Errorf("prometheus: status respons %q", qr.Status)
	}
	// Prometheus mengembalikan dua bentuk result: vector
	// (array of {metric, value}) dan scalar (array [ts, "val"]).
	// scalar() dan fungsi agregat sin * cast punya resultType "scalar", jadi
	// keduanya harus ditangani.
	var v []any
	switch qr.Data.ResultType {
	case "scalar":
		if err := json.Unmarshal(qr.Data.Result, &v); err != nil {
			return 0, fmt.Errorf("prometheus: decode scalar %q: %w", query, err)
		}
	case "vector":
		var rows []struct {
			Value []any `json:"value"`
		}
		if err := json.Unmarshal(qr.Data.Result, &rows); err != nil {
			return 0, fmt.Errorf("prometheus: decode vector %q: %w", query, err)
		}
		if len(rows) == 0 {
			return 0, fmt.Errorf("prometheus: hasil kosong untuk query %q", query)
		}
		v = rows[0].Value
	default:
		return 0, fmt.Errorf("prometheus: resultType tak didukung %q", qr.Data.ResultType)
	}
	if len(v) < 2 {
		return 0, fmt.Errorf("prometheus: format nilai tidak sesuai untuk query %q", query)
	}
	s, ok := v[1].(string)
	if !ok {
		return 0, fmt.Errorf("prometheus: tipe nilai tidak sesuai untuk query %q", query)
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("prometheus: gagal parse nilai %q: %w", s, err)
	}
	return f, nil
}

// ServerMetrics menyimpan seluruh metrik server untuk satu siklus laporan.
type ServerMetrics struct {
	UptimeDays    float64 // node_time_seconds - node_boot_time_seconds, dikonversi ke hari
	RAMTotalBytes float64 // node_memory_MemTotal_bytes
	CPUCores      float64 // count(count(node_cpu_seconds_total) by (cpu))
	CPUBusy       float64 // persen
	SysLoad       float64 // persen (node_load1 dinormalisasi jumlah core)
	RAMUsed       float64 // persen
	SwapUsed      float64 // persen
	Containers    float64 // jumlah service container running
}

// Query PromQL sesuai laporan resmi (sumber: docs/arsitektur.md dan SOP laporan).
const (
	qUptime     = "node_time_seconds - node_boot_time_seconds"
	qRAMTotal   = "node_memory_MemTotal_bytes"
	qCores      = "count(count(node_cpu_seconds_total) by (cpu))"
	qCPUBusy    = `100*(1-avg(rate(node_cpu_seconds_total{mode="idle"}[5m])))`
	qSysLoad    = "scalar(node_load1)*100/count(count(node_cpu_seconds_total) by (cpu))"
	qRAMUsed    = "(1-node_memory_MemAvailable_bytes/node_memory_MemTotal_bytes)*100"
	qSwapUsed   = "(node_memory_SwapTotal_bytes-node_memory_SwapFree_bytes)/node_memory_MemTotal_bytes*100"
	qContainers = `count(container_last_seen{name!=""})`
)

// FetchServerMetrics mengambil semua metrik server sekaligus.
// Swap yang tidak tersedia (node tanpa swap / metric absent) diperlakukan 0
// agar laporan tetap bisa dirender; metrik lain yang gagal menggagalkan siklus.
func (c *Client) FetchServerMetrics(ctx context.Context) (*ServerMetrics, error) {
	var (
		m       ServerMetrics
		uptimeS float64
		cores   float64
		load1   float64
		err     error
	)
	if uptimeS, err = c.Query(ctx, qUptime); err != nil {
		return nil, err
	}
	if m.RAMTotalBytes, err = c.Query(ctx, qRAMTotal); err != nil {
		return nil, err
	}
	if cores, err = c.Query(ctx, qCores); err != nil {
		return nil, err
	}
	if m.CPUBusy, err = c.Query(ctx, qCPUBusy); err != nil {
		return nil, err
	}
	if load1, err = c.Query(ctx, "scalar(node_load1)"); err != nil {
		return nil, err
	}
	if m.RAMUsed, err = c.Query(ctx, qRAMUsed); err != nil {
		return nil, err
	}
	if m.Containers, err = c.Query(ctx, qContainers); err != nil {
		return nil, err
	}
	// Swap opsional: node tanpa swap tidak punya metric, perlakukan 0.
	if m.SwapUsed, err = c.Query(ctx, qSwapUsed); err != nil {
		m.SwapUsed = 0
	}

	if cores <= 0 {
		cores = 1 // pengaman pembagian bila metric CPU tidak terdeteksi
	}
	m.CPUCores = cores
	m.SysLoad = load1 * 100 / cores
	m.UptimeDays = uptimeS / 86400
	return &m, nil
}
