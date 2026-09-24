// Package portalclient membaca status telemetri portal SCADATR tanpa browser:
// daftar klien MQTT beserta status broker lewat event socket.io "mqtt"
// (engine.io versi 4, transport HTTP polling, tanpa library websocket),
// cadangan REST master-rtu, dan traffic per RTU lewat event "log-mqtt" /
// "datapoint-mqtt" selama jendela pengumpulan adaptif (minimum, berhenti
// lebih awal bila semua RTU yang ditunggu sudah mengirim, maksimum).
//
// Protokol yang dipakai sudah diverifikasi dari sisi Python (skill
// scada-daily-reporting, docs/arsitektur.md):
//
//	GET  <mqtt>/socket.io/?EIO=4&transport=polling
//	     -> body diawali '0' lalu {"sid":...,"pingInterval":25000,...}
//	POST <url>&sid=<sid>, body "40"                (CONNECT namespace default)
//	POST <url>&sid=<sid>, body `42["data-mqtt"]`   (emit; dipakai halaman MQTT)
//	GET  <url>&sid=<sid> berulang                  (baca pesan/event)
//
// Pesan dalam satu body dipisah byte 0x1e. "2" adalah ping engine.io dan harus
// dibalas POST "3"; "42[...]" adalah [namaEvent, payload]. Setiap request HTTP
// membawa header Authorization: Bearer <JWT>.
package portalclient

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Default koneksi portal; dapat ditimpa lewat PORTAL_HOST, PORTAL_MQTT_PORT,
// dan PORTAL_SYSTEM_PORT.
const (
	defaultHost       = "scadatr.grita.id"
	defaultMQTTPort   = 14013
	defaultSystemPort = 14000

	// defaultMinWindow adalah lama pengumpulan traffic minimum default,
	// sama dengan default PORTAL_LOG_MIN_WINDOW_SECONDS.
	defaultMinWindow = 60 * time.Second

	// defaultMaxWindow adalah batas lama pengumpulan traffic default,
	// sama dengan default PORTAL_LOG_WINDOW_SECONDS.
	defaultMaxWindow = 600 * time.Second

	// defaultEventTimeout membatasi lamanya menunggu event "mqtt" datang.
	defaultEventTimeout = 30 * time.Second

	socketPath      = "/socket.io/"
	rtuDropdownPath = "/master-rtu/dropdown-rtu-by-protocol"
	engineVersion   = "4"

	// messageSep memisahkan beberapa pesan engine.io dalam satu body.
	messageSep = 0x1e

	maxBodyBytes = 4 << 20                // batas aman satu body polling
	emptyPollGap = 100 * time.Millisecond // jeda bila body polling kosong
)

// ErrUnauthorized dikembalikan bila portal menolak token (HTTP 401/403);
// pemanggil memperlakukannya sebagai sesi portal mati.
var ErrUnauthorized = errors.New("portalclient: token portal ditolak (HTTP 401/403)")

// Sumber daftar klien terakhir, dibaca lewat SumberKlienTerakhir. Cadangan REST
// tidak memuat status broker, jadi pemanggil harus bisa membedakannya: dengan
// cadangan itu setiap RTU tampak "no data from broker".
const (
	SumberSocket = "socket"
	SumberREST   = "rest"
)

// Config mengatur Reader. Field kosong memakai nilai default.
type Config struct {
	// Scheme default "https".
	Scheme string
	// Host default scadatr.grita.id; di container dipetakan lewat extra_hosts.
	Host string
	// MQTTPort port layanan MQTT (socket.io), default 14013.
	MQTTPort int
	// SystemPort port layanan SYSTEM (REST master-rtu), default 14000.
	SystemPort int
	// EventTimeout batas tunggu event "mqtt" pada FetchClients, default 30 detik.
	EventTimeout time.Duration
	// HTTPClient default klien dengan timeout 40 detik (long-poll engine.io
	// bisa menggantung sampai pingInterval, 25 detik).
	HTTPClient *http.Client
	// Log default slog.Default().
	Log *slog.Logger
}

// Reader membaca data portal lewat HTTP + socket.io polling.
type Reader struct {
	scheme       string
	host         string
	mqttPort     int
	systemPort   int
	eventTimeout time.Duration
	hc           *http.Client
	// pollHC dipakai khusus long-poll socket.io: tanpa Timeout klien, supaya
	// server boleh menahan GET sampai jendela pengumpulan berakhir. Deadline
	// tetap dari context tiap request.
	pollHC *http.Client
	log    *slog.Logger
	// sumberKlien mencatat dari mana daftar klien terakhir diambil
	// (SumberSocket atau SumberREST). FetchClients dipanggil satu per siklus,
	// jadi tidak perlu penguncian tambahan.
	sumberKlien string
}

// SumberKlienTerakhir melaporkan dari mana daftar klien terakhir datang:
// SumberSocket (event socket, memuat status broker) atau SumberREST (cadangan
// tanpa status broker). String kosong bila belum pernah membaca.
func (r *Reader) SumberKlienTerakhir() string {
	return r.sumberKlien
}

// New merakit Reader dari Config.
func New(cfg Config) *Reader {
	r := &Reader{
		scheme:       cfg.Scheme,
		host:         cfg.Host,
		mqttPort:     cfg.MQTTPort,
		systemPort:   cfg.SystemPort,
		eventTimeout: cfg.EventTimeout,
		hc:           cfg.HTTPClient,
		log:          cfg.Log,
	}
	if r.scheme == "" {
		r.scheme = "https"
	}
	if r.host == "" {
		r.host = defaultHost
	}
	if r.mqttPort == 0 {
		r.mqttPort = defaultMQTTPort
	}
	if r.systemPort == 0 {
		r.systemPort = defaultSystemPort
	}
	if r.eventTimeout <= 0 {
		r.eventTimeout = defaultEventTimeout
	}
	if r.hc == nil {
		r.hc = &http.Client{Timeout: 40 * time.Second}
	}
	// pollHC khusus long-poll socket.io. Server boleh menahan GET sampai jendela
	// pengumpulan berakhir (PORTAL_LOG_WINDOW_SECONDS, default 600 detik), jauh
	// lebih lama daripada batas 40 detik untuk request REST biasa. Tanpa klien
	// terpisah, long-poll gagal "context deadline exceeded" di tengah
	// pengumpulan sehingga siklus berhenti dan laporan keluar tanpa bagian
	// telemetri. Batas waktu long-poll tetap ada lewat context tiap request:
	// semua pemanggil get() memberi deadline (EventTimeout atau jendela koleksi).
	r.pollHC = &http.Client{}
	if cfg.HTTPClient != nil {
		r.pollHC.Transport = cfg.HTTPClient.Transport
	}
	if r.log == nil {
		r.log = slog.Default()
	}
	return r
}

// Station adalah ringkasan stasiun milik satu klien.
type Station struct {
	ID   string `json:"id"`
	Name string `json:"name_station"`
}

// Client adalah satu klien MQTT pada event "mqtt" (satu baris tabel
// subscriber portal). StatusMQTT true berarti broker Connected.
type Client struct {
	ID           string
	Name         string
	Protocol     string
	Host         string
	Port         string
	RTUID        int
	StationID    string
	LocationName string
	RTUOnlineAt  string
	RTUOfflineAt string
	StatusMQTT   bool
	StateMQTT    string
	Station      Station
}

// UnmarshalJSON menerima payload apa adanya dari portal: tipe field bisa
// berbeda antar entri (port angka atau string, id_rtu angka atau string,
// status_mqtt boolean atau string). Karena itu setiap field dibaca sebagai
// json.RawMessage, supaya satu tipe yang tak terduga tidak menggugurkan
// seluruh entri klien.
func (c *Client) UnmarshalJSON(b []byte) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	*c = Client{
		ID:           rawText(m["id"]),
		Name:         rawText(m["name"]),
		Protocol:     rawText(m["protocol"]),
		Host:         rawText(m["host"]),
		Port:         rawText(m["port"]),
		RTUID:        rawInt(m["id_rtu"]),
		StationID:    rawText(m["id_station"]),
		LocationName: rawText(m["loc_name_rtu"]),
		RTUOnlineAt:  rawText(m["rtu_online_at"]),
		RTUOfflineAt: rawText(m["rtu_offline_at"]),
		StatusMQTT:   rawBool(m["status_mqtt"]),
		StateMQTT:    rawText(m["state_mqtt"]),
	}
	if raw, ok := m["station"]; ok && len(raw) > 0 && raw[0] == '{' {
		var st map[string]json.RawMessage
		if err := json.Unmarshal(raw, &st); err == nil {
			c.Station = Station{ID: rawText(st["id"]), Name: rawText(st["name_station"])}
		}
	}
	return nil
}

// LogStat adalah ringkasan traffic satu RTU selama jendela pengamatan.
type LogStat struct {
	// Events adalah jumlah event log-mqtt + datapoint-mqtt untuk RTU ini.
	Events int
	// FirstTraffic adalah timestamp event pertama di jendela, apa adanya.
	FirstTraffic string
	// LastTraffic adalah timestamp event terakhir apa adanya (format panel
	// portal dd/mm/yyyy HH:MM:SS.mmm bila tersedia).
	LastTraffic string
}

// BearerToken mengubah isi SCADA_PORTAL_TOKEN / tabel portal_session (JSON
// mentah localStorage key persist:grita-scada-tr) menjadi JWT untuk header
// Bearer. Nilai yang sudah berupa JWT mentah dikembalikan apa adanya.
// Isi token tidak pernah dicetak.
func BearerToken(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("portalclient: token portal kosong")
	}
	if !strings.HasPrefix(raw, "{") {
		return raw, nil // sudah JWT mentah
	}
	var outer struct {
		Auth json.RawMessage `json:"auth"`
	}
	if err := json.Unmarshal([]byte(raw), &outer); err != nil {
		return "", fmt.Errorf("portalclient: token bukan JSON valid: %w", err)
	}
	auth := strings.TrimSpace(string(outer.Auth))
	if auth == "" || auth == "null" {
		return "", errors.New("portalclient: key auth tidak ada di dalam token")
	}
	// Nilai auth bisa berupa object atau string berisi JSON (persist redux).
	if strings.HasPrefix(auth, `"`) {
		var inner string
		if err := json.Unmarshal([]byte(auth), &inner); err != nil {
			return "", fmt.Errorf("portalclient: auth bukan string JSON valid: %w", err)
		}
		auth = strings.TrimSpace(inner)
	}
	var authObj struct {
		Token    string `json:"token"`
		UserData struct {
			Token string `json:"token"`
		} `json:"userData"`
	}
	if err := json.Unmarshal([]byte(auth), &authObj); err != nil {
		return "", fmt.Errorf("portalclient: auth tidak memuat token: %w", err)
	}
	if authObj.Token != "" {
		return authObj.Token, nil
	}
	if authObj.UserData.Token != "" {
		return authObj.UserData.Token, nil
	}
	return "", errors.New("portalclient: token JWT tidak ditemukan di dalam auth")
}

// TokenExpiry membaca klaim exp dari token portal yang tersimpan (JWT mentah
// atau blob localStorage). ok=false bila exp tidak bisa dibaca. Isi token
// tidak pernah dicetak.
func TokenExpiry(raw string) (exp time.Time, ok bool) {
	jwt, err := BearerToken(raw)
	if err != nil {
		return time.Time{}, false
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp float64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(claims.Exp), 0), true
}

// FetchClients mengembalikan daftar klien MQTT beserta status broker.
// Sumber utama adalah event socket.io "mqtt" (setelah emit "data-mqtt");
// bila event itu tidak datang, dipakai cadangan REST master-rtu yang tidak
// memuat status broker (StatusMQTT false untuk semua entri).
func (r *Reader) FetchClients(ctx context.Context, token string) ([]Client, error) {
	jwt, err := BearerToken(token)
	if err != nil {
		return nil, err
	}
	clients, err := r.fetchClientsSocket(ctx, jwt)
	if err == nil && len(clients) > 0 {
		r.sumberKlien = SumberSocket
		return clients, nil
	}
	if errors.Is(err, ErrUnauthorized) {
		return nil, err
	}
	r.log.Warn("event mqtt tidak datang dari socket, memakai cadangan REST master-rtu", "err", err)

	rest, rerr := r.fetchClientsREST(ctx, jwt)
	if rerr != nil {
		if errors.Is(rerr, ErrUnauthorized) {
			return nil, rerr
		}
		if err != nil {
			return nil, fmt.Errorf("portalclient: socket gagal (%v) dan REST gagal: %w", err, rerr)
		}
		return nil, rerr
	}
	r.sumberKlien = SumberREST
	return rest, nil
}

// CollectOptions mengatur jendela pengumpulan traffic adaptif CollectLogs.
type CollectOptions struct {
	// MinWindow adalah lama pengumpulan minimum. Setelah jendela ini lewat,
	// pengumpulan boleh berhenti lebih awal bila semua id di Expected sudah
	// punya minimal satu event. Nol atau negatif memakai default 60 detik.
	MinWindow time.Duration
	// MaxWindow adalah batas lama pengumpulan: pengumpulan berhenti begitu
	// jendela ini tercapai walau masih ada id di Expected yang belum
	// mengirim. Nol atau negatif memakai default 600 detik; nilai yang lebih
	// pendek daripada MinWindow dinaikkan agar sama.
	MaxWindow time.Duration
	// Expected adalah id_rtu yang ditunggu (mis. RTU dengan broker
	// Connected). Daftar kosong berarti tidak ada target berhenti lebih
	// awal, jadi pengumpulan menunggu sampai MaxWindow.
	Expected []int
	// OnProgress dipanggil setiap putaran polling dengan jumlah id di Expected
	// yang sudah mengirim minimal satu event dan jumlah yang ditunggu. Dipakai
	// dashboard untuk progress bar; boleh nil.
	OnProgress func(collected, expected int)
}

// CollectLogs mengumpulkan event traffic (log-mqtt + datapoint-mqtt) selama
// jendela adaptif dan mengembalikan ringkasannya per id_rtu: sekurangnya
// opt.MinWindow, berhenti lebih awal begitu semua id di opt.Expected sudah
// mengirim, dan paling lama opt.MaxWindow supaya RTU yang jarang mengirim
// tidak salah masuk kategori tanpa data.
func (r *Reader) CollectLogs(ctx context.Context, token string, opt CollectOptions) (map[int]LogStat, error) {
	jwt, err := BearerToken(token)
	if err != nil {
		return nil, err
	}
	minWindow, maxWindow := normalizeWindows(opt.MinWindow, opt.MaxWindow)

	s, err := r.dialSocket(ctx, jwt)
	if err != nil {
		return nil, err
	}
	if err := s.post(ctx, emitDataMQTT); err != nil {
		return nil, err
	}

	start := time.Now()
	minDeadline := start.Add(minWindow)
	maxDeadline := start.Add(maxWindow)

	stats := make(map[int]LogStat)
	if opt.OnProgress != nil {
		// Keadaan awal: belum ada RTU yang mengirim pada jendela ini.
		opt.OnProgress(countSeen(stats, opt.Expected), len(opt.Expected))
	}
pollLoop:
	for {
		if !time.Now().Before(maxDeadline) {
			break
		}
		// Sebelum jendela minimum lewat, long-poll dibatasi ke jendela
		// minimum saja supaya berhenti lebih awal tidak menunggu ping
		// engine.io berikutnya.
		pollUntil := maxDeadline
		if time.Now().Before(minDeadline) {
			pollUntil = minDeadline
		}
		pctx, pcancel := context.WithDeadline(ctx, pollUntil)
		events, err := s.nextEvents(pctx)
		pcancel()
		if err != nil {
			if ctx.Err() != nil {
				return nil, err
			}
			if !time.Now().Before(maxDeadline) {
				break pollLoop // jendela maksimum habis di tengah long-poll
			}
			if minDeadline.Before(maxDeadline) && !time.Now().Before(minDeadline) {
				// Batas jendela minimum tercapai di tengah long-poll: bukan
				// kegagalan, lanjut menunggu RTU yang belum mengirim.
				continue
			}
			return nil, err
		}
		for _, ev := range events {
			switch ev.name {
			case "log-mqtt":
				var le struct {
					Time    string          `json:"time"`
					RTUID   json.RawMessage `json:"id_rtu"`
					Logging struct {
						Timestamp json.RawMessage `json:"timestamp"`
					} `json:"logging"`
				}
				if err := json.Unmarshal(ev.payload, &le); err != nil {
					r.log.Warn("event log-mqtt tidak terbaca", "err", err)
					continue
				}
				ts := le.Time
				if strings.TrimSpace(ts) == "" {
					ts = rawText(le.Logging.Timestamp)
				}
				addLogStat(stats, rawInt(le.RTUID), ts)
			case "datapoint-mqtt":
				var de struct {
					RTUID     json.RawMessage `json:"id_rtu"`
					Timestamp json.RawMessage `json:"timestamp"`
				}
				if err := json.Unmarshal(ev.payload, &de); err != nil {
					r.log.Warn("event datapoint-mqtt tidak terbaca", "err", err)
					continue
				}
				addLogStat(stats, rawInt(de.RTUID), rawText(de.Timestamp))
			}
		}
		if opt.OnProgress != nil {
			opt.OnProgress(countSeen(stats, opt.Expected), len(opt.Expected))
		}
		if !time.Now().Before(minDeadline) && allSeen(stats, opt.Expected) {
			// Semua RTU yang ditunggu sudah mengirim: laporan bisa selesai
			// tanpa menunggu jendela maksimum.
			break
		}
	}
	return stats, nil
}

// countSeen menghitung berapa id di expected yang sudah punya minimal satu
// event traffic. Berguna untuk progres pengumpulan (mis. "14/22 RTU").
func countSeen(stats map[int]LogStat, expected []int) int {
	seen := 0
	for _, id := range expected {
		if stats[id].Events > 0 {
			seen++
		}
	}
	return seen
}

// allSeen melaporkan apakah setiap id di expected sudah punya minimal satu
// event traffic. Daftar kosong dianggap belum tentu semua terlihat, sehingga
// pengumpulan tetap menunggu sampai jendela maksimum.
func allSeen(stats map[int]LogStat, expected []int) bool {
	if len(expected) == 0 {
		return false
	}
	for _, id := range expected {
		if stats[id].Events == 0 {
			return false
		}
	}
	return true
}

// normalizeWindows menerapkan default jendela minimum/maksimum dan memastikan
// jendela maksimum tidak lebih pendek daripada jendela minimum.
func normalizeWindows(minWindow, maxWindow time.Duration) (time.Duration, time.Duration) {
	if minWindow <= 0 {
		minWindow = defaultMinWindow
	}
	if maxWindow <= 0 {
		maxWindow = defaultMaxWindow
	}
	if maxWindow < minWindow {
		maxWindow = minWindow
	}
	return minWindow, maxWindow
}

// addLogStat menambah hitungan event, timestamp pertama, dan timestamp terakhir.
func addLogStat(stats map[int]LogStat, rtuID int, ts string) {
	if rtuID == 0 {
		return // id_rtu tidak terbaca: jangan campur ke RTU lain
	}
	st := stats[rtuID]
	st.Events++
	if ts != "" && (st.FirstTraffic == "" || newerTraffic(st.FirstTraffic, ts)) {
		st.FirstTraffic = ts
	}
	if newerTraffic(ts, st.LastTraffic) {
		st.LastTraffic = ts
	}
	stats[rtuID] = st
}

// fetchClientsSocket menunggu event "mqtt" dari socket.io.
func (r *Reader) fetchClientsSocket(ctx context.Context, jwt string) ([]Client, error) {
	s, err := r.dialSocket(ctx, jwt)
	if err != nil {
		return nil, err
	}
	if err := s.post(ctx, emitDataMQTT); err != nil {
		return nil, err
	}

	wctx, cancel := context.WithTimeout(ctx, r.eventTimeout)
	defer cancel()
	for {
		if wctx.Err() != nil {
			return nil, fmt.Errorf("portalclient: event mqtt tidak datang dalam %s: %w", r.eventTimeout, wctx.Err())
		}
		events, err := s.nextEvents(wctx)
		if err != nil {
			return nil, err
		}
		for _, ev := range events {
			if ev.name != "mqtt" {
				continue
			}
			clients, err := r.decodeClients(ev.payload)
			if err != nil {
				r.log.Warn("payload event mqtt tidak terbaca", "err", err)
				continue
			}
			if len(clients) == 0 {
				continue
			}
			return clients, nil
		}
	}
}

// decodeClients mem-parsing payload event "mqtt": array klien. Elemen yang
// gagal di-parse dilewati supaya satu entri rusak tidak menggugurkan semua.
func (r *Reader) decodeClients(payload json.RawMessage) ([]Client, error) {
	if len(payload) == 0 {
		return nil, errors.New("payload event mqtt kosong")
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(payload, &raws); err != nil {
		return nil, fmt.Errorf("payload event mqtt bukan array: %w", err)
	}
	out := make([]Client, 0, len(raws))
	skipped := 0
	var firstErr error
	for _, raw := range raws {
		var c Client
		if err := json.Unmarshal(raw, &c); err != nil {
			skipped++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		out = append(out, c)
	}
	if skipped > 0 {
		r.log.Warn("sebagian entri klien dilewati", "dilewati", skipped, "total", len(raws),
			"err", firstErr)
	}
	return out, nil
}

// fetchClientsREST mengambil daftar RTU lewat REST (tanpa status broker).
func (r *Reader) fetchClientsREST(ctx context.Context, jwt string) ([]Client, error) {
	u := fmt.Sprintf("%s://%s:%d%s", r.scheme, r.host, r.systemPort, rtuDropdownPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u,
		strings.NewReader(`{"protocol":"MQTT"}`))
	if err != nil {
		return nil, fmt.Errorf("portalclient: siapkan request master-rtu: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+jwt)

	resp, err := r.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("portalclient: request master-rtu: %w", err)
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return nil, err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("portalclient: baca master-rtu: %w", err)
	}
	var out struct {
		StatusCode int               `json:"statusCode"`
		Message    string            `json:"message"`
		Data       []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("portalclient: master-rtu bukan JSON valid: %w", err)
	}
	clients := make([]Client, 0, len(out.Data))
	for _, raw := range out.Data {
		var c Client
		if err := json.Unmarshal(raw, &c); err != nil {
			continue
		}
		clients = append(clients, c)
	}
	if len(clients) == 0 {
		return nil, fmt.Errorf("portalclient: master-rtu tidak memuat entri RTU (statusCode %d)", out.StatusCode)
	}
	return clients, nil
}

// emitDataMQTT adalah emit yang dipakai halaman MQTT portal untuk meminta
// daftar klien.
const emitDataMQTT = `42["data-mqtt"]`

// event adalah satu event socket.io yang sudah dipisah dari body polling.
type event struct {
	name    string
	payload json.RawMessage
}

// socketSession adalah satu sesi engine.io HTTP polling (EIO=4).
type socketSession struct {
	reader *Reader
	base   string
	token  string
	sid    string
	postMu sync.Mutex // POST tidak boleh saling menumpuk
}

// dialSocket melakukan handshake lalu CONNECT namespace default.
func (r *Reader) dialSocket(ctx context.Context, jwt string) (*socketSession, error) {
	s := &socketSession{
		reader: r,
		base:   fmt.Sprintf("%s://%s:%d%s", r.scheme, r.host, r.mqttPort, socketPath),
		token:  jwt,
	}
	body, err := s.get(ctx)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 || body[0] != '0' {
		return nil, fmt.Errorf("portalclient: handshake socket.io tidak dikenal: %q", truncate(body, 80))
	}
	var open struct {
		SID          string `json:"sid"`
		PingInterval int    `json:"pingInterval"`
		PingTimeout  int    `json:"pingTimeout"`
	}
	if err := json.Unmarshal(body[1:], &open); err != nil {
		return nil, fmt.Errorf("portalclient: handshake socket.io bukan JSON valid: %w", err)
	}
	if open.SID == "" {
		return nil, errors.New("portalclient: sid kosong pada handshake socket.io")
	}
	s.sid = open.SID
	if err := s.post(ctx, "40"); err != nil {
		return nil, err
	}
	return s, nil
}

// urlPolling merakit URL polling, dengan sid bila sesi sudah terbuka.
func (s *socketSession) urlPolling() string {
	u := s.base + "?EIO=" + engineVersion + "&transport=polling"
	if s.sid != "" {
		u += "&sid=" + url.QueryEscape(s.sid)
	}
	return u
}

// get melakukan satu GET polling (long-poll) dan mengembalikan body mentah.
func (s *socketSession) get(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.urlPolling(), nil)
	if err != nil {
		return nil, fmt.Errorf("portalclient: siapkan GET socket.io: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+s.token)

	resp, err := s.reader.pollHC.Do(req)
	if err != nil {
		return nil, fmt.Errorf("portalclient: GET socket.io: %w", err)
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return nil, err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("portalclient: baca body socket.io: %w", err)
	}
	return body, nil
}

// post mengirim satu pesan engine.io/socket.io (mis. "40", "3", emit).
func (s *socketSession) post(ctx context.Context, payload string) error {
	s.postMu.Lock()
	defer s.postMu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.urlPolling(),
		strings.NewReader(payload))
	if err != nil {
		return fmt.Errorf("portalclient: siapkan POST socket.io: %w", err)
	}
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("Authorization", "Bearer "+s.token)

	resp, err := s.reader.hc.Do(req)
	if err != nil {
		return fmt.Errorf("portalclient: POST socket.io: %w", err)
	}
	defer resp.Body.Close()
	if err := checkStatus(resp); err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyBytes))
	return nil
}

// nextEvents melakukan satu GET polling dan mengembalikan event yang
// ditemukan. Ping engine.io dibalas langsung; pesan kontrol lain diabaikan.
func (s *socketSession) nextEvents(ctx context.Context) ([]event, error) {
	body, err := s.get(ctx)
	if err != nil {
		return nil, err
	}
	msgs := bytes.Split(body, []byte{messageSep})
	var events []event
	nonEmpty := 0
	for _, msg := range msgs {
		msg = bytes.TrimSpace(msg)
		if len(msg) == 0 {
			continue
		}
		nonEmpty++
		switch {
		case msg[0] == '2': // ping engine.io -> balas pong
			if err := s.post(ctx, "3"); err != nil {
				return nil, err
			}
		case bytes.HasPrefix(msg, []byte("42")):
			var arr []json.RawMessage
			if err := json.Unmarshal(msg[2:], &arr); err != nil {
				s.reader.log.Warn("pesan event socket.io tidak terbaca", "err", err)
				continue
			}
			if len(arr) == 0 {
				continue
			}
			var name string
			if err := json.Unmarshal(arr[0], &name); err != nil {
				s.reader.log.Warn("nama event socket.io tidak terbaca", "err", err)
				continue
			}
			ev := event{name: name}
			if len(arr) > 1 {
				ev.payload = arr[1]
			}
			events = append(events, ev)
		case bytes.HasPrefix(msg, []byte("44")): // connect_error
			return nil, fmt.Errorf("portalclient: socket.io connect_error: %s", truncate(msg[2:], 120))
		case msg[0] == '1': // close
			return nil, errors.New("portalclient: koneksi socket.io ditutup server")
		}
		// "0" (open), "40" (connect ack), "41" (disconnect), "3" (pong) diabaikan.
	}
	if nonEmpty == 0 {
		// Server mengembalikan body kosong: jangan berputar tanpa jeda.
		select {
		case <-ctx.Done():
		case <-time.After(emptyPollGap):
		}
	}
	return events, nil
}

// checkStatus menerjemahkan status HTTP menjadi error yang bermakna.
func checkStatus(resp *http.Response) error {
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return ErrUnauthorized
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return fmt.Errorf("portalclient: status HTTP %d dari portal", resp.StatusCode)
	}
	return nil
}

// newerTraffic melaporkan apakah ts lebih baru daripada current. Timestamp
// dibandingkan sebagai waktu bila bentuknya dikenal, jika tidak secara
// leksikografis (format panel dd/mm/yyyy berlebar tetap).
func newerTraffic(ts, current string) bool {
	ts = strings.TrimSpace(ts)
	if ts == "" {
		return false
	}
	if current == "" {
		return true
	}
	nt, nok := parseTrafficTime(ts)
	ct, cok := parseTrafficTime(current)
	if nok && cok {
		return nt.After(ct)
	}
	return ts > current
}

// layoutTraffic adalah bentuk timestamp yang dipakai portal.
var layoutTraffic = []string{
	"02/01/2006 15:04:05.000",
	"02/01/2006 15:04:05",
	"02/01/2006 15:04",
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
}

// parseTrafficTime mem-parsing timestamp portal; ok=false bila bentuknya
// tidak dikenal (nilai mentah tetap disimpan apa adanya).
func parseTrafficTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range layoutTraffic {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// rawText mengubah nilai JSON apa pun menjadi teks tanpa tanda kutip.
func rawText(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	if strings.HasPrefix(s, `"`) {
		var out string
		if err := json.Unmarshal(raw, &out); err == nil {
			return out
		}
	}
	return s
}

// rawInt mengubah nilai JSON menjadi int; nilai tak terbaca menjadi 0.
func rawInt(raw json.RawMessage) int {
	s := rawText(raw)
	if s == "" {
		return 0
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return int(f)
	}
	return 0
}

// rawBool mengubah nilai JSON menjadi bool; hanya true/1 yang dianggap benar.
func rawBool(raw json.RawMessage) bool {
	switch strings.ToLower(rawText(raw)) {
	case "true", "1":
		return true
	}
	return false
}

// truncate memotong teks panjang untuk pesan log/error.
func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}
