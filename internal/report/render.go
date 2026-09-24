// Package report merender laporan harian SCADATR UP2D JAYA sesuai format
// resmi operator (sumber: docs/arsitektur.md). Tanpa emoji, teks polos.
package report

import (
	"fmt"
	"math"
	"strings"
	"time"

	// time/tzdata menanam basis data zona waktu ke binary agar laporan tetap
	// benar bila zoneinfo sistem tidak tersedia (mis. di image minimal).
	_ "time/tzdata"
)

var jakarta = mustJakarta()

func mustJakarta() *time.Location {
	loc, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		// Cadangan bila tzdata embed gagal dimuat (seharusnya tidak terjadi).
		return time.FixedZone("WIB", 7*3600)
	}
	return loc
}

var (
	hariIndonesia  = [...]string{"Minggu", "Senin", "Selasa", "Rabu", "Kamis", "Jumat", "Sabtu"}
	bulanIndonesia = [...]string{
		"Januari", "Februari", "Maret", "April", "Mei", "Juni",
		"Juli", "Agustus", "September", "Oktober", "November", "Desember",
	}
)

// ServerMetrics berisi angka server untuk bagian STATUS KONDISI SERVER.
type ServerMetrics struct {
	UptimeDays    float64 // dalam hari (sudah dikonversi dari detik oleh prometheus client)
	RAMTotalBytes float64 // byte, dibagi 1024^3 dan dilabel GiB saat render
	CPUCores      float64
	CPUBusy       float64 // persen
	SysLoad       float64 // persen
	RAMUsed       float64 // persen
	SwapUsed      float64 // persen
	Containers    int
}

// Station berisi status telemetri satu RTU/lambung untuk bagian
// STATUS TELEMETRI RTU. Gunakan nilai kolom Station portal sebagai nama.
type Station struct {
	Station       string
	TelemetryRows int
	// LastTraffic adalah timestamp traffic terakhir apa adanya dari panel
	// (panel menampilkan UTC; nilai mentah dipertahankan, tidak dikonversi).
	// Dipakai untuk log/progres saja, tidak dirender ke laporan.
	LastTraffic string
}

// Render merender laporan lengkap sesuai format resmi operator.
// Waktu diubah ke zona Asia/Jakarta sebelum ditampilkan.
func Render(now time.Time, srv ServerMetrics, stations []Station) string {
	t := now.In(jakarta)

	var b strings.Builder
	fmt.Fprintf(&b, "Laporan Harian SCADATR UP2D JAYA\n")
	fmt.Fprintf(&b, "Hari/Tanggal: %s, %d %s %d\n",
		hariIndonesia[t.Weekday()], t.Day(), bulanIndonesia[int(t.Month())-1], t.Year())
	fmt.Fprintf(&b, "Jam: %02d:%02d\n\n", t.Hour(), t.Minute())

	fmt.Fprintf(&b, "[ STATUS KONDISI SERVER ]\n")
	fmt.Fprintf(&b, "[ Server: rocky-server ]\n")
	fmt.Fprintf(&b, "* Uptime: %d days\n", int(math.Floor(srv.UptimeDays)))
	fmt.Fprintf(&b, "* RAM Total: %d GiB\n", int(math.Round(srv.RAMTotalBytes/(1024*1024*1024))))
	fmt.Fprintf(&b, "* CPU Cores: %d\n", int(srv.CPUCores))
	fmt.Fprintf(&b, "* CPU Busy: %.1f%% / 100%%\n", srv.CPUBusy)
	fmt.Fprintf(&b, "* Sys Load: %.1f%% / 100%%\n", srv.SysLoad)
	fmt.Fprintf(&b, "* RAM Used: %.1f%% / 100%%\n", srv.RAMUsed)
	fmt.Fprintf(&b, "* SWAP Used: %.1f%% / 100%%\n", srv.SwapUsed)
	fmt.Fprintf(&b, "* Service Container Running: %d\n\n", srv.Containers)

	fmt.Fprintf(&b, "[ STATUS TELEMETRI RTU ]\n")

	active := make([]Station, 0, len(stations))
	noData := make([]Station, 0, len(stations))
	for _, s := range stations {
		if s.TelemetryRows > 0 {
			active = append(active, s)
		} else {
			noData = append(noData, s)
		}
	}

	fmt.Fprintf(&b, "  1. Lambung Active (%d):\n", len(active))
	for _, s := range active {
		fmt.Fprintf(&b, "* %s\n", s.Station)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "  2. Lambung no data from broker (%d):\n", len(noData))
	for _, s := range noData {
		fmt.Fprintf(&b, "* %s\n", s.Station)
	}

	// Laporan pesan WhatsApp tidak diakhiri baris kosong.
	return strings.TrimRight(b.String(), "\n")
}
