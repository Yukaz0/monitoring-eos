package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// newTestStore membuka SQLite in-memory untuk pengujian.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open("file::memory:?cache=shared")
	if err != nil {
		t.Fatalf("buka sqlite in-memory: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRecipientCRUD(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Create.
	r1, err := s.CreateRecipient(ctx, "Operator A", "628111111111", true)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if r1.ID == 0 || r1.Phone != "628111111111" || !r1.Active {
		t.Fatalf("recipient tidak sesuai: %+v", r1)
	}
	r2, err := s.CreateRecipient(ctx, "Operator B", "628222222222", false)
	if err != nil {
		t.Fatalf("create kedua: %v", err)
	}

	// Read (list).
	all, err := s.ListRecipients(ctx, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("jumlah recipient = %d, want 2", len(all))
	}
	activeOnly, err := s.ListRecipients(ctx, true)
	if err != nil {
		t.Fatalf("list aktif: %v", err)
	}
	if len(activeOnly) != 1 || activeOnly[0].ID != r1.ID {
		t.Fatalf("filter aktif salah: %+v", activeOnly)
	}

	// Get satu.
	got, err := s.GetRecipient(ctx, r1.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Name != "Operator A" {
		t.Fatalf("get name = %q", got.Name)
	}

	// Update.
	newActive := true
	up, err := s.UpdateRecipient(ctx, r2.ID, "Operator B2", "628333333333", &newActive)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if up.Name != "Operator B2" || up.Phone != "628333333333" || !up.Active {
		t.Fatalf("hasil update tidak sesuai: %+v", up)
	}

	// Update partial (active saja).
	partial, err := s.UpdateRecipient(ctx, r2.ID, "", "", &newActive)
	if err != nil {
		t.Fatalf("update partial: %v", err)
	}
	if partial.Name != "Operator B2" || partial.Phone != "628333333333" {
		t.Fatalf("update partial mengubah field lain: %+v", partial)
	}

	// Delete.
	if err := s.DeleteRecipient(ctx, r2.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetRecipient(ctx, r2.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get setelah delete = %v, want ErrNotFound", err)
	}
	if err := s.DeleteRecipient(ctx, r2.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete dua kali = %v, want ErrNotFound", err)
	}
}

func TestScheduleUpsert(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	sc, err := s.UpsertSchedule(ctx, "laporan-pagi", "40 7 * * *", true)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if sc.CronExpr != "40 7 * * *" || !sc.Active {
		t.Fatalf("schedule awal salah: %+v", sc)
	}

	// Upsert ulang nama sama: update, bukan duplikat.
	sc2, err := s.UpsertSchedule(ctx, "laporan-pagi", "40 15 * * *", false)
	if err != nil {
		t.Fatalf("upsert kedua: %v", err)
	}
	if sc2.ID != sc.ID || sc2.CronExpr != "40 15 * * *" || sc2.Active {
		t.Fatalf("upsert kedua tidak meng-update: %+v vs %+v", sc2, sc)
	}

	list, err := s.ListSchedules(ctx, false)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("jumlah schedule = %d, want 1", len(list))
	}

	// Mark run.
	if err := s.MarkScheduleRun(ctx, sc.ID, time.Now()); err != nil {
		t.Fatalf("mark run: %v", err)
	}
	got, err := s.GetScheduleByName(ctx, "laporan-pagi")
	if err != nil {
		t.Fatalf("get by name: %v", err)
	}
	if !got.LastRunAt.Valid {
		t.Fatal("last_run_at tidak terisi")
	}
}

func TestReportHistoryDanSendLog(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	id1, err := s.SaveReport(ctx, time.Now().Add(-time.Hour), "laporan lama")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	id2, err := s.SaveReport(ctx, time.Now(), "laporan baru")
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	latest, err := s.LatestReport(ctx)
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if latest.ID != id2 || latest.Content != "laporan baru" {
		t.Fatalf("latest salah: %+v", latest)
	}

	hist, err := s.ListReports(ctx, 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 2 || hist[0].ID != id2 {
		t.Fatalf("urutan history salah: %+v", hist)
	}

	if err := s.LogSend(ctx, SendEntry{
		ReportID:    id1,
		RecipientID: 1,
		Phone:       "628111111111",
		Status:      "sent",
	}, time.Now()); err != nil {
		t.Fatalf("log send: %v", err)
	}
	if err := s.LogSend(ctx, SendEntry{
		ReportID: id1, RecipientID: 2, Phone: "628222222222",
		Status: "error", Error: "timeout",
	}, time.Now()); err != nil {
		t.Fatalf("log send error: %v", err)
	}
}

func TestPortalSession(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	// Awal: baris seed ada, token kosong.
	ps, err := s.GetPortalSession(ctx)
	if err != nil {
		t.Fatalf("get awal: %v", err)
	}
	if ps.Token != "" || ps.Valid {
		t.Fatalf("sesi awal tidak kosong: %+v", ps)
	}

	if err := s.SavePortalToken(ctx, `{"auth":"{}"}`, true); err != nil {
		t.Fatalf("save token: %v", err)
	}
	ps, err = s.GetPortalSession(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ps.Token != `{"auth":"{}"}` || !ps.Valid || !ps.UpdatedAt.Valid {
		t.Fatalf("sesi tidak tersimpan benar: %+v", ps)
	}

	// Mark invalid: token tetap.
	if err := s.MarkPortalSession(ctx, false); err != nil {
		t.Fatalf("mark: %v", err)
	}
	ps, _ = s.GetPortalSession(ctx)
	if ps.Token != `{"auth":"{}"}` || ps.Valid {
		t.Fatalf("mark invalid salah: %+v", ps)
	}
}

// TestPortalLoginGuard menguji round-trip cooldown login persisten:
// catat -> aktif -> lewat -> hapus -> bersih.
func TestPortalLoginGuard(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	base := time.Now()
	if _, _, ok, err := s.LoginCooldown(ctx, base); err != nil || ok {
		t.Fatalf("cooldown awal = ok %v err %v, mau false/nil", ok, err)
	}

	sampai := base.Add(2 * time.Hour)
	if err := s.CatatLoginGagal(ctx, sampai, "kredensial salah"); err != nil {
		t.Fatalf("catat: %v", err)
	}
	got, pesan, ok, err := s.LoginCooldown(ctx, base)
	if err != nil || !ok {
		t.Fatalf("cooldown aktif = ok %v err %v, mau true/nil", ok, err)
	}
	if diff := got.Sub(sampai); diff < -time.Second || diff > time.Second {
		t.Fatalf("sampai = %v, mau sekitar %v", got, sampai)
	}
	if pesan != "kredensial salah" {
		t.Fatalf("pesan = %q, mau ringkasan tersimpan", pesan)
	}

	// Setelah sampai: cooldown lewat, boleh mencoba lagi.
	if _, _, ok, err := s.LoginCooldown(ctx, sampai.Add(time.Minute)); err != nil || ok {
		t.Fatalf("cooldown lewat = ok %v err %v, mau false/nil", ok, err)
	}

	if err := s.HapusLoginCooldown(ctx); err != nil {
		t.Fatalf("hapus: %v", err)
	}
	if _, pesan, ok, err := s.LoginCooldown(ctx, base); err != nil || ok || pesan != "" {
		t.Fatalf("setelah hapus = ok %v pesan %q err %v, mau false/''/nil", ok, pesan, err)
	}
}
