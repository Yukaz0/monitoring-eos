package cycle

import (
	"context"
	"testing"
	"time"

	"github.com/grita/iconics-eos-monitoring/internal/portalclient"
)

func TestClassifySocket(t *testing.T) {
	clients := []portalclient.Client{
		{RTUID: 12012000, StatusMQTT: true, Station: portalclient.Station{Name: "UPS.01.FUY"}},
		{RTUID: 12012001, StatusMQTT: true, Station: portalclient.Station{Name: "UPS.08.FUY"}},
		{RTUID: 12012002, StatusMQTT: false, Station: portalclient.Station{Name: "UPS.09.FUY"}},
		{RTUID: 12012003, StatusMQTT: true, Name: "UPS 10 FUY"},
	}
	logs := map[int]portalclient.LogStat{
		12012000: {Events: 3, LastTraffic: "23/09/2026 10:00:00.000"},
		12012002: {Events: 5, LastTraffic: "23/09/2026 10:00:01.000"},
		12012003: {Events: 0},
	}

	got := classifySocket(clients, logs)
	if len(got) != len(clients) {
		t.Fatalf("jumlah stasiun = %d, mau %d", len(got), len(clients))
	}

	want := []struct {
		nama string
		rows int
		last string
	}{
		{"UPS.01.FUY", 1, "23/09/2026 10:00:00.000"}, // status_mqtt true + ada event = Active
		{"UPS.08.FUY", 0, ""},                        // status_mqtt true tanpa event = no data
		{"UPS.09.FUY", 0, ""},                        // broker tidak connected = no data
		{"UPS 10 FUY", 0, ""},                        // nama stasiun kosong -> pakai name klien
	}
	for i, w := range want {
		if got[i].Station != w.nama || got[i].TelemetryRows != w.rows || got[i].LastTraffic != w.last {
			t.Errorf("stasiun %d = %+v, mau %s rows %d last %q", i, got[i], w.nama, w.rows, w.last)
		}
	}
}

// TestClassifySocketTimestampApaAdanya memastikan timestamp portal dipakai
// mentah (tanpa konversi zona) dan RTU no data tidak diberi timestamp.
func TestClassifySocketTimestampApaAdanya(t *testing.T) {
	clients := []portalclient.Client{
		{RTUID: 12012000, StatusMQTT: true, Station: portalclient.Station{Name: "UPS.01.FUY"}},
		{RTUID: 12012001, StatusMQTT: false, Station: portalclient.Station{Name: "UPS.08.FUY"}},
	}
	logs := map[int]portalclient.LogStat{
		12012000: {Events: 4, LastTraffic: "23/09/2026 04:21:36.403"},
		12012001: {Events: 4, LastTraffic: "23/09/2026 04:21:36.403"},
	}
	got := classifySocket(clients, logs)
	if got[0].LastTraffic != "23/09/2026 04:21:36.403" {
		t.Errorf("timestamp Active = %q, mau nilai mentah portal", got[0].LastTraffic)
	}
	if got[1].TelemetryRows != 0 || got[1].LastTraffic != "" {
		t.Errorf("RTU broker mati harus no data tanpa timestamp: %+v", got[1])
	}
}

func TestConnectedRTUs(t *testing.T) {
	clients := []portalclient.Client{
		{RTUID: 12012000, StatusMQTT: true},
		{RTUID: 12012001, StatusMQTT: false},
		{RTUID: 0, StatusMQTT: true},
		{RTUID: 12012002, StatusMQTT: true},
	}
	got := connectedRTUs(clients)
	if len(got) != 2 || got[0] != 12012000 || got[1] != 12012002 {
		t.Errorf("connectedRTUs = %v, mau [12012000 12012002]", got)
	}
	if len(connectedRTUs(nil)) != 0 {
		t.Errorf("tanpa klien harus kosong")
	}
}

func TestClassifySocketTanpaKlien(t *testing.T) {
	got := classifySocket(nil, nil)
	if len(got) != 0 {
		t.Fatalf("tanpa klien harus kosong: %+v", got)
	}
}

func TestPortalWindowDariEnv(t *testing.T) {
	t.Setenv("PORTAL_LOG_WINDOW_SECONDS", "40")
	if got := portalWindow(); got != 40*time.Second {
		t.Errorf("portalWindow = %s, mau 40s", got)
	}
	t.Setenv("PORTAL_LOG_WINDOW_SECONDS", "bukan-angka")
	if got := portalWindow(); got != defaultPortalWindowSeconds*time.Second {
		t.Errorf("portalWindow nilai rusak = %s, mau default %ds", got, defaultPortalWindowSeconds)
	}
}

func TestPortalMinWindowDariEnv(t *testing.T) {
	if defaultPortalMinWindowSeconds != 60 {
		t.Errorf("default jendela minimum = %ds, mau 60s", defaultPortalMinWindowSeconds)
	}
	t.Setenv("PORTAL_LOG_MIN_WINDOW_SECONDS", "90")
	if got := portalMinWindow(); got != 90*time.Second {
		t.Errorf("portalMinWindow = %s, mau 90s", got)
	}
	t.Setenv("PORTAL_LOG_MIN_WINDOW_SECONDS", "0")
	if got := portalMinWindow(); got != defaultPortalMinWindowSeconds*time.Second {
		t.Errorf("portalMinWindow nilai 0 = %s, mau default %ds", got, defaultPortalMinWindowSeconds)
	}
}

func TestDefaultPortalWindow(t *testing.T) {
	if defaultPortalWindowSeconds != 300 {
		t.Errorf("default jendela maksimum = %ds, mau 300s", defaultPortalWindowSeconds)
	}
}

// TestPortalIdleTimeoutDariEnv: PORTAL_LOG_IDLE_SECONDS dibaca apa adanya -
// kosong memakai default, 0 mematikan fitur, negatif dianggap 0, dan nilai
// rusak kembali ke default.
func TestPortalIdleTimeoutDariEnv(t *testing.T) {
	t.Setenv("PORTAL_LOG_IDLE_SECONDS", "")
	if got := portalIdle(); got != defaultPortalIdleSeconds*time.Second {
		t.Errorf("portalIdle kosong = %s, mau default %ds", got, defaultPortalIdleSeconds)
	}
	t.Setenv("PORTAL_LOG_IDLE_SECONDS", "90")
	if got := portalIdle(); got != 90*time.Second {
		t.Errorf("portalIdle 90 = %s, mau 90s", got)
	}
	t.Setenv("PORTAL_LOG_IDLE_SECONDS", "0")
	if got := portalIdle(); got != 0 {
		t.Errorf("portalIdle 0 = %s, mau 0 (fitur mati)", got)
	}
	t.Setenv("PORTAL_LOG_IDLE_SECONDS", "-5")
	if got := portalIdle(); got != 0 {
		t.Errorf("portalIdle -5 = %s, mau 0", got)
	}
	t.Setenv("PORTAL_LOG_IDLE_SECONDS", "bukan-angka")
	if got := portalIdle(); got != defaultPortalIdleSeconds*time.Second {
		t.Errorf("portalIdle nilai rusak = %s, mau default %ds", got, defaultPortalIdleSeconds)
	}
}

// TestCycleProgress memastikan progres pengumpulan (dipakai progress bar
// dashboard) awalnya kosong lalu terisi dari callback pengumpul log.
func TestCycleProgress(t *testing.T) {
	c := &Cycle{}
	if collected, expected, aktif := c.Progress(); collected != 0 || expected != 0 || aktif {
		t.Errorf("progres awal = %d/%d aktif=%v, mau 0/0 false", collected, expected, aktif)
	}
	c.catatProgress(14, 22)
	collected, expected, _ := c.Progress()
	if collected != 14 || expected != 22 {
		t.Errorf("setelah catatProgress = %d/%d, mau 14/22", collected, expected)
	}
}

func TestEnvIntOr(t *testing.T) {
	t.Setenv("CYCLE_TEST_INT", "12")
	if got := envIntOr("CYCLE_TEST_INT", 7); got != 12 {
		t.Errorf("envIntOr = %d, mau 12", got)
	}
	t.Setenv("CYCLE_TEST_INT", "0")
	if got := envIntOr("CYCLE_TEST_INT", 7); got != 7 {
		t.Errorf("envIntOr nilai 0 = %d, mau default 7", got)
	}
	if got := envIntOr("CYCLE_TEST_INT_TIDAK_ADA", 7); got != 7 {
		t.Errorf("envIntOr kosong = %d, mau default 7", got)
	}
}

// fakeSender mencatat pemanggilan pengiriman supaya keputusan menahan
// pengiriman bisa diuji tanpa menyentuh WhatsApp.
type fakeSender struct {
	calls  int
	lastID int64
}

func (f *fakeSender) SendReportToRecipients(_ context.Context, reportID int64, _ string) {
	f.calls++
	f.lastID = reportID
}

// TestKirimLaporanDitahanBilaTelemetriGagal: laporan tanpa bagian telemetri
// tidak boleh sampai ke recipient (hanya disimpan), sesuai keputusan operator.
func TestKirimLaporanDitahanBilaTelemetriGagal(t *testing.T) {
	sender := &fakeSender{}
	c := &Cycle{WASender: sender}

	if dikirim := c.kirimLaporan(context.Background(), false, 42, "laporan metrik saja"); dikirim {
		t.Errorf("laporan tanpa telemetri seharusnya ditahan")
	}
	if sender.calls != 0 {
		t.Errorf("pengirim dipanggil %d kali, mau 0", sender.calls)
	}
	if boleh, alasan := c.pengirimanDiizinkan(false); boleh || alasan == "" {
		t.Errorf("pengirimanDiizinkan(false) = (%v, %q), mau (false, alasan terisi)", boleh, alasan)
	}
}

// TestKirimLaporanJalanBilaTelemetriOK: jalur normal tetap mengirim.
func TestKirimLaporanJalanBilaTelemetriOK(t *testing.T) {
	sender := &fakeSender{}
	c := &Cycle{WASender: sender}

	if dikirim := c.kirimLaporan(context.Background(), true, 7, "laporan penuh"); !dikirim {
		t.Errorf("laporan dengan telemetri harus dikirim")
	}
	if sender.calls != 1 || sender.lastID != 7 {
		t.Errorf("pengirim dipanggil %d kali untuk report_id %d, mau 1 kali id 7", sender.calls, sender.lastID)
	}
	if boleh, alasan := c.pengirimanDiizinkan(true); !boleh || alasan != "" {
		t.Errorf("pengirimanDiizinkan(true) = (%v, %q), mau (true, \"\")", boleh, alasan)
	}
}

// TestKirimLaporanJalanSaatOverride: SEND_WITHOUT_TELEMETRY=1 melonggarkan
// penahanan sementara (mis. sesi portal sedang diperbarui manual).
func TestKirimLaporanJalanSaatOverride(t *testing.T) {
	sender := &fakeSender{}
	c := &Cycle{WASender: sender, SendWithoutTelemetry: true}

	if dikirim := c.kirimLaporan(context.Background(), false, 9, "laporan metrik saja"); !dikirim {
		t.Errorf("override aktif seharusnya tetap mengirim")
	}
	if sender.calls != 1 {
		t.Errorf("pengirim dipanggil %d kali, mau 1", sender.calls)
	}
	boleh, alasan := c.pengirimanDiizinkan(false)
	if !boleh || alasan != "SEND_WITHOUT_TELEMETRY=1" {
		t.Errorf("pengirimanDiizinkan override = (%v, %q)", boleh, alasan)
	}
}

// TestSkipPortalTidakLolosTanpaOverride: SKIP_PORTAL berarti telemetri tidak
// dibaca, jadi tanpa override pengiriman tetap ditahan.
func TestSkipPortalTidakLolosTanpaOverride(t *testing.T) {
	sender := &fakeSender{}
	c := &Cycle{WASender: sender, SkipPortal: true}
	if dikirim := c.kirimLaporan(context.Background(), !c.SkipPortal, 11, "laporan metrik saja"); dikirim {
		t.Errorf("SKIP_PORTAL tanpa override seharusnya ditahan")
	}
	if sender.calls != 0 {
		t.Errorf("pengirim dipanggil %d kali, mau 0", sender.calls)
	}
}
