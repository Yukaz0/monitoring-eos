package report

import (
	"testing"
	"time"
)

func TestRenderFormatPersis(t *testing.T) {
	// Kamis, 5 Maret 2026, 07:40 WIB.
	loc := time.FixedZone("WIB", 7*3600)
	now := time.Date(2026, time.March, 5, 7, 40, 0, 0, loc)

	srv := ServerMetrics{
		UptimeDays:    123.7,
		RAMTotalBytes: 93 * 1024 * 1024 * 1024, // 93 GiB persis
		CPUCores:      64,
		CPUBusy:       12.3456,
		SysLoad:       8.5,
		RAMUsed:       45.67,
		SwapUsed:      0,
		Containers:    21,
	}
	stations := []Station{
		{Station: "UPS.01.FUY", TelemetryRows: 12, LastTraffic: "23/09/2026 04:21:36.403"},
		{Station: "PV KC BRI G.SAHARI", TelemetryRows: 0},
		{Station: "IUY", TelemetryRows: 3},
	}

	got := Render(now, srv, stations)

	want := `Laporan Harian SCADATR UP2D JAYA
Hari/Tanggal: Kamis, 5 Maret 2026
Jam: 07:40

[ STATUS KONDISI SERVER ]
[ Server: rocky-server ]
* Uptime: 123 days
* RAM Total: 93 GiB
* CPU Cores: 64
* CPU Busy: 12.3% / 100%
* Sys Load: 8.5% / 100%
* RAM Used: 45.7% / 100%
* SWAP Used: 0.0% / 100%
* Service Container Running: 21

[ STATUS TELEMETRI RTU ]
  1. Lambung Active (2):
* UPS.01.FUY
* IUY

  2. Lambung no data from broker (1):
* PV KC BRI G.SAHARI`

	if got != want {
		t.Errorf("render tidak persis.\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderDaftarKosong(t *testing.T) {
	loc := time.FixedZone("WIB", 7*3600)
	now := time.Date(2026, time.March, 5, 7, 40, 0, 0, loc)

	got := Render(now, ServerMetrics{RAMTotalBytes: 1024 * 1024 * 1024}, nil)

	// Nol harus tetap tampil dengan nilai 0.
	if !contains(got, "* Uptime: 0 days") ||
		!contains(got, "* RAM Total: 1 GiB") ||
		!contains(got, "  1. Lambung Active (0):") ||
		!contains(got, "  2. Lambung no data from broker (0):") {
		t.Errorf("render daftar kosong tidak sesuai:\n%s", got)
	}
}

func TestRenderZonaJakarta(t *testing.T) {
	// 2026-03-05 22:30 UTC = 2026-03-06 05:30 WIB (Jumat).
	utc := time.Date(2026, time.March, 5, 22, 30, 0, 0, time.UTC)

	got := Render(utc, ServerMetrics{}, nil)

	if !contains(got, "Hari/Tanggal: Jumat, 6 Maret 2026") {
		t.Errorf("konversi zona Asia/Jakarta salah:\n%s", got)
	}
	if !contains(got, "Jam: 05:30") {
		t.Errorf("jam WIB salah:\n%s", got)
	}
}

// TestRenderTanpaTimestampTraffic memastikan timestamp traffic tidak lagi ikut
// dirender (keputusan operator: format laporan kembali seperti biasa), termasuk
// untuk stasiun Active yang datanya tersedia di sisi pengumpul.
func TestRenderTanpaTimestampTraffic(t *testing.T) {
	loc := time.FixedZone("WIB", 7*3600)
	now := time.Date(2026, time.March, 5, 7, 40, 0, 0, loc)

	got := Render(now, ServerMetrics{}, []Station{
		{Station: "UPS.WAPRES.01", TelemetryRows: 1, LastTraffic: "23/09/2026 11:20:20.000"},
		{Station: "UPS.08.FUY", TelemetryRows: 0, LastTraffic: "23/09/2026 04:00:00.000"},
	})

	if contains(got, "traffic terakhir") {
		t.Errorf("timestamp traffic tidak boleh dirender lagi:\n%s", got)
	}
	if contains(got, "Catatan:") {
		t.Errorf("baris Catatan tidak boleh ada lagi:\n%s", got)
	}
	if !contains(got, "* UPS.WAPRES.01") || !contains(got, "* UPS.08.FUY") {
		t.Errorf("nama stasiun harus tetap dirender:\n%s", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		(func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		})())
}
