// Helper akses sesi portal tersimpan untuk package cycle.
package cycle

import (
	"context"

	"github.com/grita/iconics-eos-monitoring/internal/store"
)

// PortalTokenFromStore membaca token portal tersimpan dari SQLite
// (tabel portal_session, diisi via PUT /api/session/token). Dipakai
// cmd/api bila env SCADA_PORTAL_TOKEN tidak di-set. Nilai token tidak
// pernah dicetak ke log.
func PortalTokenFromStore(ctx context.Context, st *store.Store) (string, bool) {
	ps, err := st.GetPortalSession(ctx)
	if err != nil || ps.Token == "" {
		return "", false
	}
	return ps.Token, true
}
