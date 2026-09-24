package portalclient

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"
)

const testJWT = "test.jwt.token"

// fakePortal meniru portal: handshake engine.io polling, CONNECT, emit, lalu
// body GET yang disiapkan test (pesan dipisah byte 0x1e).
type fakePortal struct {
	jwt           string
	unauthorized  bool
	failHandshake bool
	getResponses  []string // body GET dengan sid, berurutan; habis berarti ""
	restData      string
	restStatus    int

	mu         sync.Mutex
	getCount   int
	postBodies []string
	restHits   int
}

func (f *fakePortal) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if req.URL.Path == rtuDropdownPath {
		f.restHits++
		if req.Header.Get("Authorization") != "Bearer "+f.jwt {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if f.restStatus != 0 {
			w.WriteHeader(f.restStatus)
			return
		}
		if f.restData == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, f.restData)
		return
	}
	if req.URL.Path != socketPath {
		http.NotFound(w, req)
		return
	}
	if f.unauthorized || req.Header.Get("Authorization") != "Bearer "+f.jwt {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	switch req.Method {
	case http.MethodGet:
		if req.URL.Query().Get("sid") == "" {
			if f.failHandshake {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = io.WriteString(w,
				`0{"sid":"testsid","upgrades":["websocket"],"pingInterval":25000,"pingTimeout":60000,"maxPayload":1000000}`)
			return
		}
		f.getCount++
		if f.getCount <= len(f.getResponses) {
			_, _ = io.WriteString(w, f.getResponses[f.getCount-1])
		}
	case http.MethodPost:
		body, _ := io.ReadAll(req.Body)
		f.postBodies = append(f.postBodies, string(body))
		_, _ = io.WriteString(w, "ok")
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *fakePortal) posted() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.postBodies...)
}

func (f *fakePortal) restCalled() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.restHits
}

// newFake merakit server tiruan + Reader yang menunjuk ke sana.
func newFake(t *testing.T, f *fakePortal, eventTimeout time.Duration) (*httptest.Server, *Reader) {
	t.Helper()
	if f.jwt == "" {
		f.jwt = testJWT
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse URL httptest: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port httptest: %v", err)
	}
	r := New(Config{
		Scheme:       u.Scheme,
		Host:         u.Hostname(),
		MQTTPort:     port,
		SystemPort:   port,
		EventTimeout: eventTimeout,
		HTTPClient:   srv.Client(),
		Log:          testLogger(),
	})
	return srv, r
}

// TestPollingTidakTerpotongTimeoutKlien: server boleh menahan long-poll socket.io
// jauh lebih lama daripada timeout request REST biasa. Pengumpulan telemetri
// harus memakai batas dari context, bukan batas klien REST: tanpa itu long-poll
// gagal "context deadline exceeded" di tengah jendela dan laporan keluar tanpa
// bagian telemetri RTU (kejadian nyata 2026-09-24).
func TestPollingTidakTerpotongTimeoutKlien(t *testing.T) {
	const tahan = 250 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != socketPath {
			http.NotFound(w, req)
			return
		}
		if req.Method == http.MethodPost {
			_, _ = io.WriteString(w, "ok")
			return
		}
		if req.URL.Query().Get("sid") == "" {
			_, _ = io.WriteString(w, `0{"sid":"lambat","pingInterval":25000,"pingTimeout":60000}`)
			return
		}
		time.Sleep(tahan) // menahan long-poll lebih lama daripada timeout klien REST
		_, _ = io.WriteString(w,
			`42["mqtt",[{"id_rtu":12000016,"name":"UPS 01","host":"192.168.3.51","port":1883,"status_mqtt":true,"station":{"name_station":"UPS.01.DUY"}}]]`)
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse URL httptest: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port httptest: %v", err)
	}
	r := New(Config{
		Scheme:       u.Scheme,
		Host:         u.Hostname(),
		MQTTPort:     port,
		SystemPort:   port,
		EventTimeout: 3 * time.Second,
		// Klien REST sengaja jauh lebih pendek daripada tahanan server.
		HTTPClient: &http.Client{Timeout: 50 * time.Millisecond},
		Log:        testLogger(),
	})

	klien, err := r.FetchClients(context.Background(), testJWT)
	if err != nil {
		t.Fatalf("FetchClients: %v (long-poll tidak boleh mewarisi timeout klien REST)", err)
	}
	if len(klien) != 1 || klien[0].RTUID != 12000016 || !klien[0].StatusMQTT {
		t.Fatalf("klien = %+v, mau satu klien Connected", klien)
	}
}

// mqttPayload adalah bentuk nyata event "mqtt": dua klien, port angka dan
// string, status_mqtt true dan false.
const mqttPayload = `[
  {"id":"c1","name":"UPS 01 FUY","protocol":"MQTT","host":"192.168.3.10","port":1883,
   "id_rtu":12012000,"id_station":"st1","loc_name_rtu":"FUY",
   "rtu_online_at":"2026-09-23T00:00:00Z","rtu_offline_at":null,
   "status_mqtt":true,"state_mqtt":"subscribed","subscriptions":3,
   "station":{"id":"st1","name_station":"UPS.01.FUY"}},
  {"id":"c2","name":"UPS 08 FUY","protocol":"MQTT","host":"192.168.3.11","port":"1883",
   "id_rtu":12012001,"id_station":"st2","loc_name_rtu":"FUY",
   "rtu_online_at":"2026-09-23T00:00:00Z","rtu_offline_at":null,
   "status_mqtt":false,"state_mqtt":"offline","subscriptions":0,
   "station":{"id":"st2","name_station":"UPS.08.FUY"}}
]`

func TestFetchClientsSocket(t *testing.T) {
	f := &fakePortal{getResponses: []string{
		// connect ack + ping engine.io dalam satu body.
		"40{\"sid\":\"testsid\"}\x1e2",
		`42["mqtt",` + mqttPayload + `]`,
	}}
	_, r := newFake(t, f, 2*time.Second)

	clients, err := r.FetchClients(context.Background(), testJWT)
	if err != nil {
		t.Fatalf("FetchClients: %v", err)
	}
	if len(clients) != 2 {
		t.Fatalf("jumlah klien = %d, mau 2", len(clients))
	}
	if clients[0].RTUID != 12012000 || clients[0].Station.Name != "UPS.01.FUY" {
		t.Errorf("klien pertama tidak sesuai: %+v", clients[0])
	}
	if !clients[0].StatusMQTT {
		t.Errorf("status_mqtt klien pertama harus true")
	}
	if clients[0].Port != "1883" {
		t.Errorf("port angka harus terbaca sebagai teks, dapat %q", clients[0].Port)
	}
	if clients[1].StatusMQTT {
		t.Errorf("status_mqtt klien kedua harus false")
	}
	if clients[1].Port != "1883" {
		t.Errorf("port string harus terbaca, dapat %q", clients[1].Port)
	}
	if clients[1].RTUID != 12012001 {
		t.Errorf("id_rtu klien kedua = %d", clients[1].RTUID)
	}

	posts := f.posted()
	if len(posts) < 3 {
		t.Fatalf("POST yang tercatat hanya %v", posts)
	}
	if posts[0] != "40" {
		t.Errorf("POST pertama harus CONNECT \"40\", dapat %q", posts[0])
	}
	if posts[1] != emitDataMQTT {
		t.Errorf("POST kedua harus emit %q, dapat %q", emitDataMQTT, posts[1])
	}
	foundPong := false
	for _, p := range posts {
		if p == "3" {
			foundPong = true
		}
	}
	if !foundPong {
		t.Errorf("ping engine.io tidak dibalas POST \"3\"; POST: %v", posts)
	}
	if f.restCalled() != 0 {
		t.Errorf("REST cadangan tidak boleh dipanggil saat socket sukses")
	}
}

func TestFetchClientsTipeFieldAneh(t *testing.T) {
	// Regresi: entri klien nyata bisa memakai tipe berbeda (id angka,
	// id_rtu string, status_mqtt string) dan tetap harus terbaca.
	payload := `[
	  {"id":7,"name":"UPS 01 FUY","protocol":"MQTT","host":"192.168.3.10","port":1883,
	   "id_rtu":"12012000","id_station":12,"loc_name_rtu":null,
	   "rtu_online_at":{"ts":"2026-09-23T00:00:00Z"},"rtu_offline_at":"",
	   "status_mqtt":"true","state_mqtt":"subscribed","subscriptions":[1,2],
	   "station":{"id":1,"name_station":"UPS.01.FUY"}}
	]`
	f := &fakePortal{getResponses: []string{
		`40{"sid":"testsid"}`,
		`42["mqtt",` + payload + `]`,
	}}
	_, r := newFake(t, f, 2*time.Second)

	clients, err := r.FetchClients(context.Background(), testJWT)
	if err != nil {
		t.Fatalf("FetchClients: %v", err)
	}
	if len(clients) != 1 {
		t.Fatalf("jumlah klien = %d, mau 1", len(clients))
	}
	c := clients[0]
	if c.RTUID != 12012000 || !c.StatusMQTT || c.Station.Name != "UPS.01.FUY" {
		t.Errorf("entri dengan tipe campur tidak terbaca benar: %+v", c)
	}
	if c.Port != "1883" {
		t.Errorf("port = %q", c.Port)
	}
}

func TestFetchClientsFallbackREST(t *testing.T) {
	f := &fakePortal{
		failHandshake: true,
		restData: `{"statusCode":200,"message":"sukses","data":[` +
			`{"id":"r1","id_rtu":12012000,"protocol":"MQTT","id_station":"st1",` +
			`"station":{"name_station":"UPS.01.FUY"}}]}`,
	}
	_, r := newFake(t, f, time.Second)

	clients, err := r.FetchClients(context.Background(), testJWT)
	if err != nil {
		t.Fatalf("FetchClients: %v", err)
	}
	if len(clients) != 1 || clients[0].RTUID != 12012000 {
		t.Fatalf("hasil cadangan REST tidak sesuai: %+v", clients)
	}
	if clients[0].StatusMQTT {
		t.Errorf("cadangan REST tidak memuat status broker, harus false")
	}
	if f.restCalled() != 1 {
		t.Errorf("REST cadangan dipanggil %d kali, mau 1", f.restCalled())
	}
}

func TestFetchClientsFallbackSaatEventMqttTidakDatang(t *testing.T) {
	f := &fakePortal{
		// Hanya connect ack, event mqtt tidak pernah datang.
		getResponses: []string{`40{"sid":"testsid"}`},
		restData: `{"statusCode":200,"message":"sukses","data":[` +
			`{"id":"r1","id_rtu":12012009,"protocol":"MQTT","id_station":"st9",` +
			`"station":{"name_station":"UPS.09.FUY"}}]}`,
	}
	_, r := newFake(t, f, 300*time.Millisecond)

	clients, err := r.FetchClients(context.Background(), testJWT)
	if err != nil {
		t.Fatalf("FetchClients: %v", err)
	}
	if len(clients) != 1 || clients[0].RTUID != 12012009 {
		t.Fatalf("harus jatuh ke cadangan REST: %+v", clients)
	}
}

func TestFetchClientsTokenDitolak(t *testing.T) {
	f := &fakePortal{unauthorized: true}
	_, r := newFake(t, f, time.Second)

	_, err := r.FetchClients(context.Background(), testJWT)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("error = %v, mau ErrUnauthorized", err)
	}
	if f.restCalled() != 0 {
		t.Errorf("REST tidak perlu dicoba saat token ditolak")
	}
}

func TestCollectLogs(t *testing.T) {
	f := &fakePortal{getResponses: []string{
		`40{"sid":"testsid"}`,
		`42["log-mqtt",{"time":"23/09/2026 10:00:01.100","name":"UPS 01 FUY","id_rtu":12012000,"topic":"a/b","logging":{"timestamp":"23/09/2026 10:00:01.100"}}]` +
			"\x1e" +
			`42["log-mqtt",{"time":"23/09/2026 09:55:00.000","name":"UPS 01 FUY","id_rtu":12012000,"topic":"a/b","logging":{"timestamp":"23/09/2026 09:55:00.000"}}]`,
		`42["datapoint-mqtt",{"id_rtu":12012001,"type":0,"value":{"a":1},"quality":"good","timestamp":"23/09/2026 10:00:02.000"}]`,
		`42["mqtt",` + mqttPayload + `]`, // event lain harus diabaikan
	}}
	_, r := newFake(t, f, time.Second)

	stats, err := r.CollectLogs(context.Background(), testJWT, CollectOptions{
		MinWindow: 100 * time.Millisecond,
		MaxWindow: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("CollectLogs: %v", err)
	}
	first, ok := stats[12012000]
	if !ok {
		t.Fatalf("RTU 12012000 tidak tercatat: %+v", stats)
	}
	if first.Events != 2 {
		t.Errorf("event RTU 12012000 = %d, mau 2", first.Events)
	}
	if first.LastTraffic != "23/09/2026 10:00:01.100" {
		t.Errorf("timestamp terakhir RTU 12012000 = %q, mau nilai terbaru", first.LastTraffic)
	}
	if first.FirstTraffic != "23/09/2026 09:55:00.000" {
		t.Errorf("timestamp pertama RTU 12012000 = %q, mau nilai terlama", first.FirstTraffic)
	}
	second, ok := stats[12012001]
	if !ok || second.Events != 1 {
		t.Fatalf("RTU 12012001 harus punya 1 event datapoint: %+v", stats)
	}
	if second.LastTraffic != "23/09/2026 10:00:02.000" {
		t.Errorf("timestamp datapoint = %q", second.LastTraffic)
	}
	if _, ada := stats[0]; ada {
		t.Errorf("RTU tanpa id_rtu tidak boleh tercatat")
	}
}

func TestCountSeen(t *testing.T) {
	stats := map[int]LogStat{1: {Events: 2}, 2: {Events: 0}}
	if got := countSeen(stats, []int{1, 2, 3}); got != 1 {
		t.Errorf("countSeen = %d, mau 1", got)
	}
	if got := countSeen(stats, nil); got != 0 {
		t.Errorf("countSeen(nil) = %d, mau 0", got)
	}
}

// TestCollectLogsProgress memastikan OnProgress melaporkan jumlah RTU yang
// sudah mengirim dari yang ditunggu - sumber data progress bar dashboard.
func TestCollectLogsProgress(t *testing.T) {
	f := &fakePortal{getResponses: []string{
		`40{"sid":"testsid"}`,
		`42["log-mqtt",{"time":"23/09/2026 10:00:01.100","id_rtu":12012000,"logging":{"timestamp":"23/09/2026 10:00:01.100"}}]`,
		`42["log-mqtt",{"time":"23/09/2026 10:00:02.100","id_rtu":12012001,"logging":{"timestamp":"23/09/2026 10:00:02.100"}}]`,
	}}
	_, r := newFake(t, f, time.Second)

	var mu sync.Mutex
	var calls [][2]int
	_, err := r.CollectLogs(context.Background(), testJWT, CollectOptions{
		MinWindow: 100 * time.Millisecond,
		MaxWindow: 300 * time.Millisecond,
		// RTU 12012002 tidak pernah mengirim, jadi pengumpulan menunggu
		// jendela maksimum dan progres berhenti di 2/3.
		Expected: []int{12012000, 12012001, 12012002},
		OnProgress: func(collected, expected int) {
			mu.Lock()
			calls = append(calls, [2]int{collected, expected})
			mu.Unlock()
		},
	})
	if err != nil {
		t.Fatalf("CollectLogs: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) == 0 {
		t.Fatal("OnProgress tidak pernah dipanggil")
	}
	if awal := calls[0]; awal[0] != 0 || awal[1] != 3 {
		t.Errorf("progres awal = %v, mau {0 3}", awal)
	}
	if akhir := calls[len(calls)-1]; akhir[0] != 2 || akhir[1] != 3 {
		t.Errorf("progres akhir = %v, mau {2 3}", akhir)
	}
}

func TestCollectLogsJendelaKosong(t *testing.T) {
	f := &fakePortal{getResponses: []string{`40{"sid":"testsid"}`}}
	_, r := newFake(t, f, time.Second)

	stats, err := r.CollectLogs(context.Background(), testJWT, CollectOptions{
		MinWindow: 50 * time.Millisecond,
		MaxWindow: 150 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("CollectLogs: %v", err)
	}
	if len(stats) != 0 {
		t.Fatalf("jendela tanpa event harus kosong: %+v", stats)
	}
}

// TestCollectLogsBerhentiLebihAwal memastikan pengumpulan selesai begitu
// jendela minimum lewat DAN semua id di Expected sudah mengirim, tanpa
// menunggu jendela maksimum.
func TestCollectLogsBerhentiLebihAwal(t *testing.T) {
	f := &fakePortal{getResponses: []string{
		`40{"sid":"testsid"}`,
		`42["log-mqtt",{"time":"23/09/2026 10:00:01.100","id_rtu":12012000}]` + "\x1e" +
			`42["datapoint-mqtt",{"id_rtu":12012001,"timestamp":"23/09/2026 10:00:02.000"}]`,
	}}
	_, r := newFake(t, f, time.Second)

	const minWindow = 150 * time.Millisecond
	const maxWindow = 5 * time.Second
	start := time.Now()
	stats, err := r.CollectLogs(context.Background(), testJWT, CollectOptions{
		MinWindow: minWindow,
		MaxWindow: maxWindow,
		Expected:  []int{12012000, 12012001},
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("CollectLogs: %v", err)
	}
	if stats[12012000].Events != 1 || stats[12012001].Events != 1 {
		t.Fatalf("event kedua RTU Expected harus tercatat: %+v", stats)
	}
	if elapsed < minWindow {
		t.Errorf("pengumpulan hanya %s, harus menunggu jendela minimum %s", elapsed, minWindow)
	}
	if elapsed >= maxWindow {
		t.Errorf("pengumpulan %s tidak berhenti lebih awal dari jendela maksimum %s", elapsed, maxWindow)
	}
}

// TestCollectLogsTetapMenungguJendelaMaksimum memastikan RTU yang belum
// pernah mengirim tetap ditunggu sampai jendela maksimum walau RTU lain sudah
// mengirim, supaya tidak salah masuk no data from broker.
func TestCollectLogsTetapMenungguJendelaMaksimum(t *testing.T) {
	f := &fakePortal{getResponses: []string{
		`40{"sid":"testsid"}`,
		`42["log-mqtt",{"time":"23/09/2026 10:00:01.100","id_rtu":12012000}]`,
	}}
	_, r := newFake(t, f, time.Second)

	const minWindow = 50 * time.Millisecond
	const maxWindow = 400 * time.Millisecond
	start := time.Now()
	stats, err := r.CollectLogs(context.Background(), testJWT, CollectOptions{
		MinWindow: minWindow,
		MaxWindow: maxWindow,
		Expected:  []int{12012000, 12012001}, // 12012001 tidak pernah mengirim
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("CollectLogs: %v", err)
	}
	if stats[12012000].Events != 1 {
		t.Fatalf("RTU yang mengirim harus tercatat: %+v", stats)
	}
	if _, ada := stats[12012001]; ada {
		t.Errorf("RTU yang tidak mengirim tidak boleh tercatat: %+v", stats)
	}
	if elapsed < maxWindow {
		t.Errorf("pengumpulan berhenti di %s, harus menunggu jendela maksimum %s", elapsed, maxWindow)
	}
	if elapsed > maxWindow+time.Second {
		t.Errorf("pengumpulan %s melewati jendela maksimum %s terlalu jauh", elapsed, maxWindow)
	}
}

// TestCollectLogsMenghormatiJendelaMinimum memastikan jendela minimum dipegang
// walau semua RTU Expected sudah mengirim lebih dulu.
func TestCollectLogsMenghormatiJendelaMinimum(t *testing.T) {
	f := &fakePortal{getResponses: []string{
		`40{"sid":"testsid"}`,
		`42["datapoint-mqtt",{"id_rtu":12012000,"timestamp":"23/09/2026 10:00:02.000"}]`,
	}}
	_, r := newFake(t, f, time.Second)

	const minWindow = 250 * time.Millisecond
	start := time.Now()
	stats, err := r.CollectLogs(context.Background(), testJWT, CollectOptions{
		MinWindow: minWindow,
		MaxWindow: 3 * time.Second,
		Expected:  []int{12012000},
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("CollectLogs: %v", err)
	}
	if stats[12012000].Events != 1 {
		t.Fatalf("event RTU Expected harus tercatat: %+v", stats)
	}
	if elapsed < minWindow {
		t.Errorf("pengumpulan hanya %s, jendela minimum %s tidak dihormati", elapsed, minWindow)
	}
	if elapsed > 2*time.Second {
		t.Errorf("pengumpulan %s terlalu lama, seharusnya berhenti setelah jendela minimum", elapsed)
	}
}

func TestNormalizeWindows(t *testing.T) {
	cases := []struct {
		nama             string
		min, max         time.Duration
		wantMin, wantMax time.Duration
	}{
		{"default", 0, 0, defaultMinWindow, defaultMaxWindow},
		{"dipakai apa adanya", 30 * time.Second, 2 * time.Minute, 30 * time.Second, 2 * time.Minute},
		{"maks lebih pendek dari min", time.Minute, 10 * time.Second, time.Minute, time.Minute},
		{"min negatif", -time.Second, time.Minute, defaultMinWindow, time.Minute},
	}
	for _, c := range cases {
		gotMin, gotMax := normalizeWindows(c.min, c.max)
		if gotMin != c.wantMin || gotMax != c.wantMax {
			t.Errorf("%s: normalizeWindows(%s, %s) = (%s, %s), mau (%s, %s)",
				c.nama, c.min, c.max, gotMin, gotMax, c.wantMin, c.wantMax)
		}
	}
}

func TestAllSeen(t *testing.T) {
	stats := map[int]LogStat{12012000: {Events: 2}, 12012001: {Events: 0}}
	if allSeen(stats, nil) {
		t.Errorf("Expected kosong tidak boleh dianggap semua sudah mengirim")
	}
	if allSeen(stats, []int{12012000, 12012001}) {
		t.Errorf("RTU tanpa event belum terlihat")
	}
	if !allSeen(stats, []int{12012000}) {
		t.Errorf("RTU dengan event harus dianggap sudah mengirim")
	}
}

func TestBearerToken(t *testing.T) {
	cases := []struct {
		nama string
		raw  string
		want string
		err  bool
	}{
		{"auth string redux", `{"auth":"{\"token\":\"JWT-A\",\"isAuthenticated\":true}","ups":{}}`, "JWT-A", false},
		{"auth object", `{"auth":{"token":"JWT-B"}}`, "JWT-B", false},
		{"auth userData", `{"auth":{"userData":{"token":"JWT-C"},"isAuthenticated":true}}`, "JWT-C", false},
		{"jwt mentah", "abc.def.ghi", "abc.def.ghi", false},
		{"kosong", "", "", true},
		{"tanpa auth", `{"ups":{}}`, "", true},
		{"auth tanpa token", `{"auth":"{\"isAuthenticated\":false}"}`, "", true},
	}
	for _, c := range cases {
		got, err := BearerToken(c.raw)
		if c.err {
			if err == nil {
				t.Errorf("%s: mau error, dapat %q", c.nama, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.nama, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: dapat %q, mau %q", c.nama, got, c.want)
		}
	}
}

func TestNewerTraffic(t *testing.T) {
	cases := []struct {
		nama    string
		ts      string
		current string
		want    bool
	}{
		{"kosong tidak menimpa", "", "23/09/2026 10:00:00.000", false},
		{"pertama kali", "23/09/2026 10:00:00.000", "", true},
		{"lebih baru", "23/09/2026 10:00:02.000", "23/09/2026 10:00:01.000", true},
		{"lebih lama", "23/09/2026 10:00:01.000", "23/09/2026 10:00:02.000", false},
		{"sama", "23/09/2026 10:00:01.000", "23/09/2026 10:00:01.000", false},
		{"lintas format", "2026-09-23T10:00:03Z", "23/09/2026 10:00:02.000", true},
		{"lintas format mundur", "2026-09-23T10:00:01Z", "23/09/2026 10:00:02.000", false},
		{"tak dikenal leksikografis", "zz", "aa", true},
	}
	for _, c := range cases {
		if got := newerTraffic(c.ts, c.current); got != c.want {
			t.Errorf("%s: newerTraffic(%q, %q) = %v, mau %v", c.nama, c.ts, c.current, got, c.want)
		}
	}
}

func TestReaderDefault(t *testing.T) {
	r := New(Config{})
	if r.scheme != "https" || r.host != defaultHost ||
		r.mqttPort != defaultMQTTPort || r.systemPort != defaultSystemPort {
		t.Errorf("default koneksi salah: %+v", r)
	}
	if r.eventTimeout != defaultEventTimeout {
		t.Errorf("default event timeout salah: %s", r.eventTimeout)
	}
}

// testLogger mengirim log ke io.Discard supaya output test tetap bersih.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestTokenExpiry memastikan exp dibaca dari JWT mentah maupun dari blob
// localStorage yang menyimpan JWT di dalam auth.
func TestTokenExpiry(t *testing.T) {
	const want = int64(1790000000)
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1790000000}`))
	jwt := "header." + payload + ".signature"

	got, ok := TokenExpiry(jwt)
	if !ok || got.Unix() != want {
		t.Errorf("JWT mentah: ok=%v exp=%v, mau ok=true exp=%d", ok, got.Unix(), want)
	}

	blob := `{"auth":{"userData":{"token":"` + jwt + `"},"isAuthenticated":true}}`
	got, ok = TokenExpiry(blob)
	if !ok || got.Unix() != want {
		t.Errorf("blob localStorage: ok=%v exp=%v, mau ok=true exp=%d", ok, got.Unix(), want)
	}

	// auth berupa string berisi JSON (bentuk persist redux).
	blobStr := `{"auth":"{\"token\":\"` + jwt + `\"}"}`
	if got, ok = TokenExpiry(blobStr); !ok || got.Unix() != want {
		t.Errorf("auth string JSON: ok=%v exp=%v, mau ok=true exp=%d", ok, got.Unix(), want)
	}

	for _, bad := range []string{"", "bukan-token", "header.!!!.sig", `{"auth":{"token":""}}`, "a.b"} {
		if _, ok := TokenExpiry(bad); ok {
			t.Errorf("TokenExpiry(%q) = ok, mau ok=false", bad)
		}
	}
}
