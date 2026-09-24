// Package store menyediakan akses SQLite (driver modernc.org/sqlite, CGO-free)
// untuk recipients, schedules, riwayat laporan, log kirim, dan sesi portal.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // driver pure-Go, di-register sebagai "sqlite"
)

// ErrNotFound dikembalikan saat record tidak ditemukan.
var ErrNotFound = errors.New("record tidak ditemukan")

// Store membungkus *sql.DB SQLite.
type Store struct {
	db *sql.DB
}

// Open membuka (atau membuat) database SQLite di path yang diberikan dan
// menjalankan migrasi skema. Contoh dsn: file:/data/monitoring.db?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)
func Open(dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: gagal buka sqlite: %w", err)
	}
	// modernc/sqlite menulis single-writer; batasi koneksi agar aman dari
	// error "database is locked" saat dipakai bersama HTTP handler + scheduler.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close menutup koneksi database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS recipients (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL DEFAULT '',
	phone TEXT NOT NULL,
	active INTEGER NOT NULL DEFAULT 1,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS schedules (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL UNIQUE,
	cron_expr TEXT NOT NULL,
	active INTEGER NOT NULL DEFAULT 1,
	last_run_at DATETIME
);
CREATE TABLE IF NOT EXISTS report_history (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	generated_at DATETIME NOT NULL,
	content TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS send_log (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	report_id INTEGER,
	recipient_id INTEGER,
	phone TEXT NOT NULL,
	status TEXT NOT NULL,
	error TEXT,
	sent_at DATETIME
);
CREATE TABLE IF NOT EXISTS portal_session (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	token TEXT,
	updated_at DATETIME,
	valid INTEGER NOT NULL DEFAULT 0
);
INSERT OR IGNORE INTO portal_session (id, token, updated_at, valid) VALUES (1, NULL, NULL, 0);
CREATE TABLE IF NOT EXISTS portal_login_guard (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	gagal_at DATETIME,
	sampai_at DATETIME,
	pesan TEXT NOT NULL DEFAULT ''
);
INSERT OR IGNORE INTO portal_login_guard (id, gagal_at, sampai_at, pesan) VALUES (1, NULL, NULL, '');
`
	_, err := s.db.Exec(schema)
	if err != nil {
		return fmt.Errorf("store: migrasi gagal: %w", err)
	}
	return nil
}

// Recipient adalah penerima laporan WhatsApp.
type Recipient struct {
	ID        int64
	Name      string
	Phone     string
	Active    bool
	CreatedAt time.Time
}

// CreateRecipient menambah recipient baru.
func (s *Store) CreateRecipient(ctx context.Context, name, phone string, active bool) (Recipient, error) {
	r := Recipient{Name: name, Phone: phone, Active: active}
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO recipients (name, phone, active) VALUES (?, ?, ?) RETURNING id, created_at`,
		name, phone, boolToInt(active)).Scan(&r.ID, &r.CreatedAt)
	if err != nil {
		return Recipient{}, fmt.Errorf("store: insert recipient: %w", err)
	}
	return r, nil
}

// ListRecipients mengembalikan semua recipient; bila onlyActive, hanya yang aktif.
func (s *Store) ListRecipients(ctx context.Context, onlyActive bool) ([]Recipient, error) {
	q := `SELECT id, name, phone, active, created_at FROM recipients`
	if onlyActive {
		q += ` WHERE active = 1`
	}
	q += ` ORDER BY id`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store: list recipients: %w", err)
	}
	defer rows.Close()

	var out []Recipient
	for rows.Next() {
		var r Recipient
		var active int
		if err := rows.Scan(&r.ID, &r.Name, &r.Phone, &active, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan recipient: %w", err)
		}
		r.Active = active == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateRecipient mengubah recipient; field kosong tidak diubah.
// Nol value pada phone (string kosong) berarti pertahankan nilai lama.
func (s *Store) UpdateRecipient(ctx context.Context, id int64, name, phone string, active *bool) (Recipient, error) {
	sets := []string{}
	args := []any{}
	if name != "" {
		sets = append(sets, "name = ?")
		args = append(args, name)
	}
	if phone != "" {
		sets = append(sets, "phone = ?")
		args = append(args, phone)
	}
	if active != nil {
		sets = append(sets, "active = ?")
		args = append(args, boolToInt(*active))
	}
	if len(sets) == 0 {
		return s.GetRecipient(ctx, id)
	}
	args = append(args, id)
	res, err := s.db.ExecContext(ctx,
		`UPDATE recipients SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return Recipient{}, fmt.Errorf("store: update recipient: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Recipient{}, ErrNotFound
	}
	return s.GetRecipient(ctx, id)
}

// GetRecipient mengambil satu recipient berdasarkan id.
func (s *Store) GetRecipient(ctx context.Context, id int64) (Recipient, error) {
	var r Recipient
	var active int
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, phone, active, created_at FROM recipients WHERE id = ?`, id).
		Scan(&r.ID, &r.Name, &r.Phone, &active, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Recipient{}, ErrNotFound
	}
	if err != nil {
		return Recipient{}, fmt.Errorf("store: get recipient: %w", err)
	}
	r.Active = active == 1
	return r, nil
}

// DeleteRecipient menghapus recipient. Mengembalikan ErrNotFound bila tidak ada.
func (s *Store) DeleteRecipient(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM recipients WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("store: delete recipient: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Schedule adalah jadwal cron laporan (zona Asia/Jakarta).
type Schedule struct {
	ID        int64
	Name      string
	CronExpr  string
	Active    bool
	LastRunAt sql.NullTime
}

// ListSchedules mengembalikan semua jadwal; bila onlyActive, hanya yang aktif.
func (s *Store) ListSchedules(ctx context.Context, onlyActive bool) ([]Schedule, error) {
	q := `SELECT id, name, cron_expr, active, last_run_at FROM schedules`
	if onlyActive {
		q += ` WHERE active = 1`
	}
	q += ` ORDER BY id`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("store: list schedules: %w", err)
	}
	defer rows.Close()

	var out []Schedule
	for rows.Next() {
		var sc Schedule
		var active int
		if err := rows.Scan(&sc.ID, &sc.Name, &sc.CronExpr, &active, &sc.LastRunAt); err != nil {
			return nil, fmt.Errorf("store: scan schedule: %w", err)
		}
		sc.Active = active == 1
		out = append(out, sc)
	}
	return out, rows.Err()
}

// UpsertSchedule menyisipkan atau memperbarui jadwal berdasarkan nama.
// name tidak boleh kosong karena dipakai sebagai kunci upsert.
func (s *Store) UpsertSchedule(ctx context.Context, name, cronExpr string, active bool) (Schedule, error) {
	if name == "" {
		return Schedule{}, fmt.Errorf("store: nama schedule wajib diisi")
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO schedules (name, cron_expr, active) VALUES (?, ?, ?)
ON CONFLICT(name) DO UPDATE SET cron_expr = excluded.cron_expr, active = excluded.active`,
		name, cronExpr, boolToInt(active))
	if err != nil {
		return Schedule{}, fmt.Errorf("store: upsert schedule: %w", err)
	}
	return s.GetScheduleByName(ctx, name)
}

// GetScheduleByName mengambil satu jadwal berdasarkan nama.
func (s *Store) GetScheduleByName(ctx context.Context, name string) (Schedule, error) {
	var sc Schedule
	var active int
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, cron_expr, active, last_run_at FROM schedules WHERE name = ?`, name).
		Scan(&sc.ID, &sc.Name, &sc.CronExpr, &active, &sc.LastRunAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Schedule{}, ErrNotFound
	}
	if err != nil {
		return Schedule{}, fmt.Errorf("store: get schedule: %w", err)
	}
	sc.Active = active == 1
	return sc, nil
}

// MarkScheduleRun menyimpan waktu eksekusi terakhir jadwal.
func (s *Store) MarkScheduleRun(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE schedules SET last_run_at = ? WHERE id = ?`, at, id)
	if err != nil {
		return fmt.Errorf("store: mark schedule run: %w", err)
	}
	return nil
}

// SaveReport menyimpan laporan hasil render dan mengembalikan id-nya.
func (s *Store) SaveReport(ctx context.Context, generatedAt time.Time, content string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO report_history (generated_at, content) VALUES (?, ?) RETURNING id`,
		generatedAt, content).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: save report: %w", err)
	}
	return id, nil
}

// Report adalah satu entri riwayat laporan.
type Report struct {
	ID          int64
	GeneratedAt time.Time
	Content     string
}

// LatestReport mengembalikan laporan terbaru berdasarkan generated_at.
func (s *Store) LatestReport(ctx context.Context) (Report, error) {
	var r Report
	err := s.db.QueryRowContext(ctx, `
SELECT id, generated_at, content FROM report_history ORDER BY generated_at DESC, id DESC LIMIT 1`).
		Scan(&r.ID, &r.GeneratedAt, &r.Content)
	if errors.Is(err, sql.ErrNoRows) {
		return Report{}, ErrNotFound
	}
	if err != nil {
		return Report{}, fmt.Errorf("store: latest report: %w", err)
	}
	return r, nil
}

// ListReports mengembalikan riwayat laporan terbaru (maks limit entri).
func (s *Store) ListReports(ctx context.Context, limit int) ([]Report, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id, generated_at, content FROM report_history ORDER BY generated_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list reports: %w", err)
	}
	defer rows.Close()

	var out []Report
	for rows.Next() {
		var r Report
		if err := rows.Scan(&r.ID, &r.GeneratedAt, &r.Content); err != nil {
			return nil, fmt.Errorf("store: scan report: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SendEntry adalah satu baris log pengiriman WhatsApp.
type SendEntry struct {
	ReportID    int64
	RecipientID int64
	Phone       string
	Status      string
	Error       string
}

// LogSend mencatat hasil satu pengiriman.
func (s *Store) LogSend(ctx context.Context, e SendEntry, sentAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO send_log (report_id, recipient_id, phone, status, error, sent_at) VALUES (?, ?, ?, ?, ?, ?)`,
		e.ReportID, e.RecipientID, e.Phone, e.Status, e.Error, sentAt)
	if err != nil {
		return fmt.Errorf("store: log send: %w", err)
	}
	return nil
}

// SavePortalToken menyimpan token portal; valid=1 berarti sesi dianggap hidup.
func (s *Store) SavePortalToken(ctx context.Context, token string, valid bool) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO portal_session (id, token, updated_at, valid) VALUES (1, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET token = excluded.token, updated_at = excluded.updated_at, valid = excluded.valid`,
		token, time.Now(), boolToInt(valid))
	if err != nil {
		return fmt.Errorf("store: save portal token: %w", err)
	}
	return nil
}

// PortalSession adalah baris tunggal tabel portal_session.
type PortalSession struct {
	Token     string
	UpdatedAt sql.NullTime
	Valid     bool
}

// GetPortalSession membaca sesi portal saat ini.
func (s *Store) GetPortalSession(ctx context.Context) (PortalSession, error) {
	var ps PortalSession
	var token sql.NullString
	var valid int
	err := s.db.QueryRowContext(ctx,
		`SELECT token, updated_at, valid FROM portal_session WHERE id = 1`).
		Scan(&token, &ps.UpdatedAt, &valid)
	if errors.Is(err, sql.ErrNoRows) {
		return PortalSession{}, ErrNotFound
	}
	if err != nil {
		return PortalSession{}, fmt.Errorf("store: get portal session: %w", err)
	}
	ps.Token = token.String
	ps.Valid = valid == 1
	return ps, nil
}

// MarkPortalSession menandai validitas sesi portal tanpa mengubah token.
func (s *Store) MarkPortalSession(ctx context.Context, valid bool) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE portal_session SET valid = ?, updated_at = ? WHERE id = 1`, boolToInt(valid), time.Now())
	if err != nil {
		return fmt.Errorf("store: mark portal session: %w", err)
	}
	return nil
}

// CatatLoginGagal menyimpan kegagalan login terakhir dan kapan percobaan
// berikutnya boleh dilakukan. pesan adalah ringkasan dari portal (tanpa
// kredensial). Cooldown bertahan lintas restart container karena disimpan di
// SQLite, sehingga siklus terjadwal tidak menghabiskan jatah percobaan akun.
func (s *Store) CatatLoginGagal(ctx context.Context, sampai time.Time, pesan string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO portal_login_guard (id, gagal_at, sampai_at, pesan) VALUES (1, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET gagal_at = excluded.gagal_at, sampai_at = excluded.sampai_at, pesan = excluded.pesan`,
		time.Now(), sampai, pesan)
	if err != nil {
		return fmt.Errorf("store: catat login gagal: %w", err)
	}
	return nil
}

// LoginCooldown membaca kapan percobaan login boleh dilakukan lagi.
// ok=false berarti tidak ada cooldown aktif (boleh mencoba). pesan tetap
// dikembalikan walau cooldown sudah lewat, supaya pemanggil bisa menampilkan
// sebab kegagalan terakhir.
func (s *Store) LoginCooldown(ctx context.Context, sekarang time.Time) (sampai time.Time, pesan string, ok bool, err error) {
	var sampaiNull sql.NullTime
	err = s.db.QueryRowContext(ctx,
		`SELECT sampai_at, pesan FROM portal_login_guard WHERE id = 1`).
		Scan(&sampaiNull, &pesan)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, "", false, nil
	}
	if err != nil {
		return time.Time{}, "", false, fmt.Errorf("store: baca cooldown login: %w", err)
	}
	if !sampaiNull.Valid || !sekarang.Before(sampaiNull.Time) {
		return time.Time{}, pesan, false, nil
	}
	return sampaiNull.Time, pesan, true, nil
}

// HapusLoginCooldown membersihkan cooldown setelah login berhasil.
func (s *Store) HapusLoginCooldown(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE portal_login_guard SET gagal_at = NULL, sampai_at = NULL, pesan = '' WHERE id = 1`)
	if err != nil {
		return fmt.Errorf("store: hapus cooldown login: %w", err)
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
