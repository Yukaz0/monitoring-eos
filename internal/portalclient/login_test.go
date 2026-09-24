package portalclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newLoginFake merakit Reader yang menunjuk ke server HTTP tiruan; hanya
// endpoint auth yang dipanggil, jadi MQTTPort tidak relevan.
func newLoginFake(t *testing.T, h http.HandlerFunc) *Reader {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse URL httptest: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port httptest: %v", err)
	}
	return New(Config{
		Scheme:     u.Scheme,
		Host:       u.Hostname(),
		SystemPort: port,
		MQTTPort:   port,
		HTTPClient: srv.Client(),
		Log:        testLogger(),
	})
}

// TestLoginTokenDiUserData: bentuk respons portal yang paling umum, token di
// data.userData.token.
func TestLoginTokenDiUserData(t *testing.T) {
	const want = "aaa.bbb.ccc"
	var authHeader string
	r := newLoginFake(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != loginPath {
			http.NotFound(w, req)
			return
		}
		authHeader = req.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"statusCode":200,"message":"sukses","data":{"userData":{"token":"` + want + `"}}}`))
	})

	hasil, err := r.Login(context.Background(), "operator", "rahasia")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if hasil.Token != want {
		t.Errorf("token = %q, mau %q", hasil.Token, want)
	}
	if hasil.DuaFaktor {
		t.Errorf("DuaFaktor harus false")
	}
	if authHeader != "" {
		t.Errorf("login tidak boleh memakai header Authorization, dapat %q", authHeader)
	}
}

// TestLoginDitolak403MembawaPesanPortal: portal memakai 403 baik untuk akun
// yang diblokir maupun penolakan lain, jadi pesan bodinya wajib ikut ke error
// supaya operator tahu sebabnya dan tidak mencoba lagi secara buta.
func TestLoginDitolak403MembawaPesanPortal(t *testing.T) {
	const pesan = "Account is blocked after too many failed attempts"
	r := newLoginFake(t, func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"statusCode":403,"message":"` + pesan + `"}`))
	})

	_, err := r.Login(context.Background(), "operator", "rahasia")
	if !errors.Is(err, ErrLoginDitolak) {
		t.Fatalf("error = %v, mau ErrLoginDitolak", err)
	}
	if !strings.Contains(err.Error(), pesan) {
		t.Errorf("pesan portal tidak ikut ke error: %v", err)
	}
	if strings.Contains(err.Error(), "rahasia") {
		t.Errorf("password bocor ke error: %v", err)
	}
}

// TestLoginDitolak401BodiKosong: bodi penolakan kosong tetap menghasilkan
// error yang bermakna, bukan panik.
func TestLoginDitolak401BodiKosong(t *testing.T) {
	r := newLoginFake(t, func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	_, err := r.Login(context.Background(), "operator", "rahasia")
	if !errors.Is(err, ErrLoginDitolak) {
		t.Fatalf("error = %v, mau ErrLoginDitolak", err)
	}
	if !strings.Contains(err.Error(), "HTTP 401") {
		t.Errorf("error tidak memuat status: %v", err)
	}
}

// TestVerify2FASudahLogin: portal grita hanya mengizinkan satu sesi per akun,
// jadi verify-2fa bisa ditolak dengan "User is already logged in". Itu bukan
// kesalahan kode 2FA/kredensial dan harus bisa dibedakan pemanggilnya.
func TestVerify2FASudahLogin(t *testing.T) {
	r := newLoginFake(t, func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"statusCode":401,"message":"User is already logged in"}`))
	})

	_, err := r.Verify2FA(context.Background(), "pre-token", "123456")
	if !errors.Is(err, ErrSudahLogin) {
		t.Fatalf("error = %v, mau ErrSudahLogin", err)
	}
	if errors.Is(err, ErrLoginDitolak) {
		t.Errorf("ErrSudahLogin tidak boleh sekaligus ErrLoginDitolak: %v", err)
	}
	if !strings.Contains(err.Error(), "already logged in") {
		t.Errorf("pesan portal tidak ikut ke error: %v", err)
	}
}

// TestLoginTokenDiAuthString: auth disimpan sebagai string berisi JSON
// (bentuk persist redux portal).
func TestLoginTokenDiAuthString(t *testing.T) {
	const want = "ddd.eee.fff"
	r := newLoginFake(t, func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte(`{"statusCode":200,"message":"ok","data":{"auth":"{\"token\":\"` + want + `\",\"isAuthenticated\":true}"}}`))
	})

	hasil, err := r.Login(context.Background(), "operator", "rahasia")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if hasil.Token != want {
		t.Errorf("token = %q, mau %q", hasil.Token, want)
	}
}

// TestLoginDuaFaktor: two_factor_required mengembalikan sinyal 2FA tanpa
// error, lengkap dengan pre_auth_token.
func TestLoginDuaFaktor(t *testing.T) {
	const preAuth = "pre.abc.def"
	r := newLoginFake(t, func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte(`{"statusCode":200,"two_factor_required":true,"pre_auth_token":"` + preAuth + `","pre_auth_expires_in":300}`))
	})

	hasil, err := r.Login(context.Background(), "operator", "rahasia")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if !hasil.DuaFaktor {
		t.Fatalf("DuaFaktor harus true: %+v", hasil)
	}
	if hasil.PreAuthToken != preAuth {
		t.Errorf("PreAuthToken = %q, mau %q", hasil.PreAuthToken, preAuth)
	}
	if hasil.PreAuthExpiresIn != 300 {
		t.Errorf("PreAuthExpiresIn = %d, mau 300", hasil.PreAuthExpiresIn)
	}
	if hasil.Token != "" {
		t.Errorf("token harus kosong saat 2FA, dapat %q", hasil.Token)
	}
}

// TestLoginDitolak401: HTTP 401 diterjemahkan menjadi ErrLoginDitolak.
func TestLoginDitolak401(t *testing.T) {
	r := newLoginFake(t, func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"statusCode":401,"message":"invalid credentials"}`))
	})

	_, err := r.Login(context.Background(), "operator", "rahasia")
	if !errors.Is(err, ErrLoginDitolak) {
		t.Fatalf("error = %v, mau ErrLoginDitolak", err)
	}
}

// TestTOTPVC6238 memverifikasi HOTP/TOTP terhadap vektor RFC 6238 (SHA1).
// Nilai digit-8 diambil apa adanya, sedangkan TOTPCode 6 digit adalah enam
// angka terakhirnya.
func TestTOTPVC6238(t *testing.T) {
	const secret = "GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ" // base32 dari "12345678901234567890"
	key, err := decodeTOTPSecret(secret)
	if err != nil {
		t.Fatalf("decodeTOTPSecret: %v", err)
	}

	cases := []struct {
		unix        int64
		wantDelapan string
		wantEnam    string
	}{
		{59, "94287082", "287082"},
		{1111111109, "07081804", "081804"},
		{1111111111, "14050471", "050471"},
		{1234567890, "89005924", "005924"},
		{2000000000, "69279037", "279037"},
	}
	for _, c := range cases {
		counter := uint64(c.unix / 30)
		if got := hotp(key, counter, 8); got != c.wantDelapan {
			t.Errorf("hotp(t=%d, digit 8) = %q, mau %q", c.unix, got, c.wantDelapan)
		}
		kode, err := TOTPCode(secret, time.Unix(c.unix, 0))
		if err != nil {
			t.Fatalf("TOTPCode(t=%d): %v", c.unix, err)
		}
		if kode != c.wantEnam {
			t.Errorf("TOTPCode(t=%d) = %q, mau %q", c.unix, kode, c.wantEnam)
		}
	}

	if _, err := TOTPCode("", time.Now()); err == nil {
		t.Errorf("TOTPCode dengan secret kosong harus error")
	}
}

// TestLoginWith2FATanpaSecret: akun 2FA tanpa secret TOTP menghasilkan
// ErrDuaFaktorDiperlukan (tidak bisa diotomasi), bukan error samar.
func TestLoginWith2FATanpaSecret(t *testing.T) {
	r := newLoginFake(t, func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte(`{"statusCode":200,"two_factor_required":true,"pre_auth_token":"pre.abc.def"}`))
	})

	_, err := r.LoginWith2FA(context.Background(), "operator", "rahasia", "")
	if !errors.Is(err, ErrDuaFaktorDiperlukan) {
		t.Fatalf("error = %v, mau ErrDuaFaktorDiperlukan", err)
	}
}

// TestLoginErrorTidakBocorkanPassword memastikan password tidak pernah ikut
// ke pesan error walau server memantulkannya.
func TestLoginErrorTidakBocorkanPassword(t *testing.T) {
	const password = "Rahasia-Sangat-Rahasia-123"
	r := newLoginFake(t, func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"statusCode":400,"message":"invalid login for ` + password + `"}`))
	})

	_, err := r.Login(context.Background(), "operator", password)
	if !errors.Is(err, ErrLoginDitolak) {
		t.Fatalf("error = %v, mau ErrLoginDitolak", err)
	}
	if strings.Contains(err.Error(), password) {
		t.Fatalf("pesan error membocorkan password: %v", err)
	}
}
