// Login REST portal SCADATR tanpa browser. Portal hanya menerbitkan token
// lewat /users/login (berlaku ~8 jam, tanpa endpoint refresh), jadi operator
// tidak perlu lagi login manual di browser tiap slot laporan. Kredensial dan
// token tidak pernah dicatat ke log/error.
package portalclient

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Path endpoint auth portal (layanan SYSTEM, port REST yang sama dengan
// master-rtu).
const (
	loginPath     = "/users/login"
	verify2FAPath = "/users/verify-2fa"
	logoutPath    = "/users/logout"

	// maxTokenDepth membatasi pencarian nilai token di dalam respons portal
	// (mis. data.userData.token ada di kedalaman tiga).
	maxTokenDepth = 3

	// totpDigits jumlah digit kode TOTP; step 30 detik, HMAC-SHA1 (RFC 6238).
	totpDigits = 6
	totpStep   = 30 * time.Second
)

// ErrLoginDitolak dikembalikan bila portal menolak kredensial/kode 2FA.
var ErrLoginDitolak = errors.New("portalclient: login ditolak portal")

// ErrDuaFaktorDiperlukan dikembalikan bila akun butuh kode 2FA tetapi secret
// TOTP tidak tersedia (atau akun belum mendaftarkan authenticator).
var ErrDuaFaktorDiperlukan = errors.New("portalclient: akun butuh kode 2FA")

// ErrSudahLogin dikembalikan bila portal menolak karena akun masih punya sesi
// aktif ("User is already logged in"). Portal hanya mengizinkan satu sesi per
// akun, jadi ini keadaan sementara — BUKAN kesalahan kredensial, dan tidak
// boleh memicu cooldown login.
var ErrSudahLogin = errors.New("portalclient: akun portal masih punya sesi aktif")

// sudahLogin melaporkan apakah pesan penolakan portal berarti sesi lama masih
// hidup. Diperiksa dari pesan bodi karena portal memakai HTTP 401 untuk kasus
// ini juga, sama seperti kredensial salah.
func sudahLogin(pesan string) bool {
	return strings.Contains(strings.ToLower(pesan), "already logged in")
}

// HasilLogin merangkum respons /users/login atau /users/verify-2fa.
type HasilLogin struct {
	Token            string
	DuaFaktor        bool // two_factor_required
	PreAuthToken     string
	PreAuthExpiresIn int
	PerluQRSetup     bool // requires_qr_setup
}

// Login memanggil POST /users/login. Bila portal meminta 2FA, DuaFaktor true
// dan PreAuthToken terisi (token boleh kosong). Kredensial tidak pernah
// dimasukkan ke pesan error.
func (r *Reader) Login(ctx context.Context, username, password string) (HasilLogin, error) {
	if strings.TrimSpace(username) == "" || password == "" {
		return HasilLogin{}, errors.New("portalclient: username dan password wajib diisi")
	}
	payload, err := json.Marshal(map[string]string{"username": username, "password": password})
	if err != nil {
		return HasilLogin{}, fmt.Errorf("portalclient: marshal bodi login: %w", err)
	}
	body, status, err := r.postAuth(ctx, loginPath, payload, "")
	if err != nil {
		return HasilLogin{}, err
	}
	// 401/403 bisa berarti kredensial salah ATAU akun diblokir setelah
	// beberapa kali gagal; pesan bodi portal dibaca lebih dulu supaya operator
	// tahu sebabnya (dan tidak mencoba lagi secara buta).
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		pesan := pesanPenolakan(body)
		if sudahLogin(pesan) {
			return HasilLogin{}, fmt.Errorf("portalclient: login ditolak (HTTP %d): %s: %w",
				status, pesan, ErrSudahLogin)
		}
		return HasilLogin{}, fmt.Errorf("portalclient: login ditolak portal (HTTP %d): %s: %w",
			status, redaksiPassword(pesan, password), ErrLoginDitolak)
	}
	resp, err := parseResponsAuth(body)
	if err != nil {
		return HasilLogin{}, err
	}

	// 2FA: kembalikan sinyal tanpa error supaya pemanggil bisa lanjut verify.
	if resp.twoFactor {
		return HasilLogin{
			DuaFaktor:        true,
			PreAuthToken:     resp.preAuth,
			PreAuthExpiresIn: resp.preAuthTTL,
			PerluQRSetup:     resp.requiresQR,
		}, nil
	}

	if ditolak, alasan := resp.ditolak(status); ditolak {
		return HasilLogin{}, fmt.Errorf("portalclient: login ditolak: %s: %w",
			redaksiPassword(alasan, password), ErrLoginDitolak)
	}

	token := cariToken(body, 0, false)
	if token == "" && !resp.requiresQR {
		return HasilLogin{}, errors.New("portalclient: respons login tidak memuat token")
	}
	return HasilLogin{Token: token, PerluQRSetup: resp.requiresQR}, nil
}

// Verify2FA menukar pre_auth_token + kode TOTP menjadi token akses.
func (r *Reader) Verify2FA(ctx context.Context, preAuthToken, kode string) (HasilLogin, error) {
	if strings.TrimSpace(preAuthToken) == "" || strings.TrimSpace(kode) == "" {
		return HasilLogin{}, errors.New("portalclient: pre_auth_token dan kode 2FA wajib diisi")
	}
	payload, err := json.Marshal(map[string]string{
		"pre_auth_token": preAuthToken,
		"totp_code":      kode,
	})
	if err != nil {
		return HasilLogin{}, fmt.Errorf("portalclient: marshal bodi verify-2fa: %w", err)
	}
	body, status, err := r.postAuth(ctx, verify2FAPath, payload, "")
	if err != nil {
		return HasilLogin{}, err
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		pesan := pesanPenolakan(body)
		if sudahLogin(pesan) {
			return HasilLogin{}, fmt.Errorf("portalclient: verifikasi 2FA ditolak (HTTP %d): %s: %w",
				status, pesan, ErrSudahLogin)
		}
		return HasilLogin{}, fmt.Errorf("portalclient: verifikasi 2FA ditolak portal (HTTP %d): %s: %w",
			status, pesan, ErrLoginDitolak)
	}
	resp, err := parseResponsAuth(body)
	if err != nil {
		return HasilLogin{}, err
	}
	if ditolak, alasan := resp.ditolak(status); ditolak {
		return HasilLogin{}, fmt.Errorf("portalclient: verifikasi 2FA ditolak: %s: %w",
			alasan, ErrLoginDitolak)
	}
	token := cariToken(body, 0, false)
	if token == "" {
		return HasilLogin{}, errors.New("portalclient: respons verify-2fa tidak memuat token")
	}
	return HasilLogin{Token: token}, nil
}

// LoginWith2FA menjalankan Login, lalu bila akun butuh 2FA, mencoba
// Verify2FA dengan kode untuk step sekarang beserta step-1 dan step+1
// (toleransi geser jam). Mengembalikan yang pertama berhasil.
func (r *Reader) LoginWith2FA(ctx context.Context, username, password, totpSecret string) (HasilLogin, error) {
	hasil, err := r.Login(ctx, username, password)
	if err != nil {
		return HasilLogin{}, err
	}
	if hasil.PerluQRSetup {
		return HasilLogin{}, fmt.Errorf(
			"portalclient: akun belum terdaftar authenticator (QR setup), tidak bisa diotomasi: %w",
			ErrDuaFaktorDiperlukan)
	}
	if !hasil.DuaFaktor {
		return hasil, nil
	}
	if strings.TrimSpace(totpSecret) == "" {
		return HasilLogin{}, fmt.Errorf(
			"portalclient: akun butuh kode 2FA tetapi PORTAL_TOTP_SECRET kosong: %w",
			ErrDuaFaktorDiperlukan)
	}

	now := time.Now()
	steps := []time.Time{now, now.Add(-totpStep), now.Add(totpStep)}
	for _, ts := range steps {
		kode, err := TOTPCode(totpSecret, ts)
		if err != nil {
			return HasilLogin{}, err
		}
		out, err := r.Verify2FA(ctx, hasil.PreAuthToken, kode)
		if err == nil {
			return out, nil
		}
		if !errors.Is(err, ErrLoginDitolak) {
			return HasilLogin{}, err
		}
		// Kode ditolak: jangan menyerah dulu, coba step waktu berikutnya.
	}
	return HasilLogin{}, fmt.Errorf("portalclient: semua kode 2FA ditolak: %w", ErrLoginDitolak)
}

// Logout memutus sesi portal. Error boleh diabaikan pemanggil karena token
// akan mati sendiri saat kedaluwarsa.
func (r *Reader) Logout(ctx context.Context, token string) error {
	jwt, err := BearerToken(token)
	if err != nil {
		return err
	}
	_, status, err := r.postAuth(ctx, logoutPath, []byte("{}"), jwt)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("portalclient: logout status HTTP %d", status)
	}
	return nil
}

// TOTPCode menghitung kode TOTP 6 digit (RFC 6238, step 30 detik, HMAC-SHA1).
// Secret base32 dinormalisasi lebih dulu (spasi/tanda hubung dibuang, huruf
// besar, padding ditambahkan). Secret kosong menghasilkan error jelas.
func TOTPCode(secret string, t time.Time) (string, error) {
	key, err := decodeTOTPSecret(secret)
	if err != nil {
		return "", err
	}
	counter := uint64(t.Unix() / int64(totpStep.Seconds()))
	return hotp(key, counter, totpDigits), nil
}

// hotp menghitung kode HOTP (dasar TOTP). Dipisah supaya bisa diuji dengan
// vektor RFC 6238 memakai digit 8.
func hotp(secret []byte, counter uint64, digits int) string {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	mac := hmac.New(sha1.New, secret)
	mac.Write(buf[:])
	sum := mac.Sum(nil)

	// Dynamic truncation (RFC 4226): offset diambil dari 4 bit terakhir.
	offset := sum[len(sum)-1] & 0x0f
	code := (uint32(sum[offset]&0x7f) << 24) |
		(uint32(sum[offset+1]) << 16) |
		(uint32(sum[offset+2]) << 8) |
		uint32(sum[offset+3])

	mod := uint32(1)
	for i := 0; i < digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", digits, int(code%mod))
}

// decodeTOTPSecret menormalkan secret base32 lalu mendekodenya menjadi kunci
// HMAC. Padding ditambahkan karena secret portal sering ditulis tanpa '='.
func decodeTOTPSecret(secret string) ([]byte, error) {
	if strings.TrimSpace(secret) == "" {
		return nil, errors.New("portalclient: secret TOTP kosong")
	}
	norm := strings.NewReplacer(" ", "", "-", "").Replace(secret)
	norm = strings.ToUpper(strings.TrimSpace(norm))
	if pad := len(norm) % 8; pad != 0 {
		norm += strings.Repeat("=", 8-pad)
	}
	key, err := base32.StdEncoding.DecodeString(norm)
	if err != nil {
		return nil, fmt.Errorf("portalclient: secret TOTP bukan base32 valid: %w", err)
	}
	if len(key) == 0 {
		return nil, errors.New("portalclient: secret TOTP kosong setelah dekode")
	}
	return key, nil
}

// postAuth mengirim JSON ke endpoint auth portal dan mengembalikan body
// mentah beserta status HTTP. bearer kosong berarti tanpa header
// Authorization (endpoint login/2FA). Isi bodi tidak pernah dicatat.
func (r *Reader) postAuth(ctx context.Context, path string, payload []byte, bearer string) ([]byte, int, error) {
	u := fmt.Sprintf("%s://%s:%d%s", r.scheme, r.host, r.systemPort, path)
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, body)
	if err != nil {
		return nil, 0, fmt.Errorf("portalclient: siapkan request %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("portalclient: request %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("portalclient: baca respons %s: %w", path, err)
	}
	return raw, resp.StatusCode, nil
}

// responsAuth adalah pembacaan toleran respons auth portal: field bisa berada
// di level atas atau di dalam "data".
type responsAuth struct {
	statusCode int
	message    string
	twoFactor  bool
	requiresQR bool
	preAuth    string
	preAuthTTL int
}

// parseResponsAuth membaca flag/pesan dari respons auth apa adanya. Field
// yang tidak terbaca dibiarkan nol supaya satu bentuk respons baru tidak
// menggagalkan seluruh login.
func parseResponsAuth(body []byte) (responsAuth, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return responsAuth{}, fmt.Errorf("portalclient: respons auth bukan JSON valid: %w", err)
	}
	data := nestedMap(m)

	resp := responsAuth{
		twoFactor:  flagDari(m, data, "two_factor_required"),
		requiresQR: flagDari(m, data, "requires_qr_setup"),
		preAuth:    teksDari(m, data, "pre_auth_token"),
		preAuthTTL: intDari(m, data, "pre_auth_expires_in"),
	}
	resp.statusCode = intDari(m, data, "statusCode")
	resp.message = teksDari(m, data, "message")
	if resp.message == "" {
		resp.message = teksDari(m, data, "error")
	}
	return resp, nil
}

// ditolak memutuskan apakah respons auth berarti kredensial/kode ditolak:
// HTTP 401/403, HTTP 400-499 lain, atau statusCode >= 400 dengan pesan yang
// memuat invalid/failed/required.
func (r responsAuth) ditolak(httpStatus int) (bool, string) {
	if httpStatus == http.StatusUnauthorized || httpStatus == http.StatusForbidden {
		return true, pesanAtauDefault(r.message)
	}
	if httpStatus >= 400 && httpStatus < 500 {
		return true, pesanAtauDefault(r.message)
	}
	if r.statusCode >= 400 {
		low := strings.ToLower(r.message)
		for _, kata := range []string{"invalid", "failed", "required"} {
			if strings.Contains(low, kata) {
				return true, pesanAtauDefault(r.message)
			}
		}
	}
	return false, ""
}

// pesanAtauDefault menghindari pesan error kosong pada penolakan.
func pesanAtauDefault(msg string) string {
	if strings.TrimSpace(msg) == "" {
		return "pesan server kosong"
	}
	return msg
}

// pesanPenolakan membaca pesan dari bodi penolakan portal. Bodi bisa JSON
// ({"message":...} / {"error":...}) atau teks biasa, dan bisa juga kosong;
// nilai kosong berarti portal tidak memberi alasan.
func pesanPenolakan(body []byte) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err == nil {
		if s := rawText(m["message"]); s != "" {
			return s
		}
		if s := rawText(m["error"]); s != "" {
			return s
		}
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200]
	}
	if s == "" {
		return "pesan server kosong"
	}
	return s
}

// redaksiPassword membuang password dari pesan server supaya kredensial
// tidak ikut bocor ke error/log walau server memantulkannya.
func redaksiPassword(pesan, password string) string {
	if password == "" {
		return pesan
	}
	return strings.ReplaceAll(pesan, password, "[disensor]")
}

// nestedMap mengembalikan objek "data" respons; data bisa berupa object atau
// string berisi JSON (bentuk persist redux portal).
func nestedMap(m map[string]json.RawMessage) map[string]json.RawMessage {
	raw, ok := m["data"]
	if !ok || len(raw) == 0 {
		return nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return nil
		}
		trimmed = bytes.TrimSpace([]byte(s))
	}
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	var dm map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &dm); err != nil {
		return nil
	}
	return dm
}

// flagDari membaca boolean dari level atas lalu fallback ke dalam data.
func flagDari(m, data map[string]json.RawMessage, key string) bool {
	if rawBool(m[key]) {
		return true
	}
	return rawBool(data[key])
}

// teksDari membaca string dari level atas lalu fallback ke dalam data.
func teksDari(m, data map[string]json.RawMessage, key string) string {
	if s := rawText(m[key]); s != "" {
		return s
	}
	return rawText(data[key])
}

// intDari membaca int dari level atas lalu fallback ke dalam data.
func intDari(m, data map[string]json.RawMessage, key string) int {
	if n := rawInt(m[key]); n != 0 {
		return n
	}
	return rawInt(data[key])
}

// cariToken menelusuri respons JSON untuk menemukan nilai token. Nilai di
// kunci token/access_token diterima apa pun isinya; kunci lain hanya
// diterima bila berbentuk JWT (3 segmen). String berisi JSON ditelusuri lagi
// (auth portal disimpan sebagai string oleh redux persist). Pencarian
// dibatasi maxTokenDepth.
func cariToken(raw json.RawMessage, depth int, tokenKey bool) string {
	if depth > maxTokenDepth || len(raw) == 0 {
		return ""
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return ""
	}
	switch trimmed[0] {
	case '{':
		var m map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &m); err != nil {
			return ""
		}
		// Kunci token diprioritaskan (bisa sedalam apa pun dalam batas).
		for _, k := range []string{"token", "access_token"} {
			if v, ok := m[k]; ok {
				if s := cariToken(v, depth+1, true); s != "" {
					return s
				}
			}
		}
		// Jalur yang dipakai portal, lalu sisa kunci secara deterministik.
		for _, k := range []string{"data", "auth", "userData"} {
			if v, ok := m[k]; ok {
				if s := cariToken(v, depth+1, false); s != "" {
					return s
				}
			}
		}
		sisa := make([]string, 0, len(m))
		for k := range m {
			switch k {
			case "token", "access_token", "data", "auth", "userData":
				continue
			}
			sisa = append(sisa, k)
		}
		sort.Strings(sisa)
		for _, k := range sisa {
			if s := cariToken(m[k], depth+1, false); s != "" {
				return s
			}
		}
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return ""
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return ""
		}
		if tokenKey || isJWT(s) {
			return s
		}
		if strings.HasPrefix(s, "{") {
			// auth disimpan sebagai string JSON: telusuri pada level yang sama.
			return cariToken(json.RawMessage(s), depth, false)
		}
	}
	return ""
}

// isJWT melaporkan apakah teks berbentuk JWT: tiga segmen dipisah titik yang
// semuanya berisi karakter base64url. Pemeriksaan karakter ini penting supaya
// string JSON (mis. auth redux persist) yang kebetulan punya dua titik tidak
// salah dianggap token.
func isJWT(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for _, c := range p {
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '=':
			default:
				return false
			}
		}
	}
	return true
}
