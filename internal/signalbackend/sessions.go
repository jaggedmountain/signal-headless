package signalbackend

import (
	"context"
	"regexp"
	"strconv"

	"go.mau.fi/mautrix-signal/pkg/libsignalgo"
)

// signalmeow encrypts to every device that has a session record, but a
// record can hold only archived states (signal-cli archives sessions of
// unlinked or reset devices; signalmeow's retry path archives too). libsignal
// then fails with "session not found" before the server can report the device
// as stale, blocking every send to that recipient — including the sync copy
// to our own devices. Removing such records lets the normal 409/410 handling
// rebuild sessions from fresh prekeys.

type deadSession struct {
	serviceID, theirServiceID string
	deviceID                  int
}

// pruneDeadSessions deletes session records that have no current state.
func (b *Backend) pruneDeadSessions(ctx context.Context) (int, error) {
	rows, err := b.db.Query(ctx, `SELECT service_id, their_service_id, their_device_id, record FROM signalmeow_sessions WHERE account_id=$1`, b.dev.ACI)
	if err != nil {
		return 0, err
	}
	var dead []deadSession
	for rows.Next() {
		var s deadSession
		var rec []byte
		if err := rows.Scan(&s.serviceID, &s.theirServiceID, &s.deviceID, &rec); err != nil {
			rows.Close()
			return 0, err
		}
		sr, err := libsignalgo.DeserializeSessionRecord(rec)
		if err != nil {
			dead = append(dead, s)
			continue
		}
		if ok, err := sr.HasCurrentState(); err == nil && !ok {
			dead = append(dead, s)
		}
	}
	rows.Close()
	for _, s := range dead {
		_, err := b.db.Exec(ctx, `DELETE FROM signalmeow_sessions WHERE account_id=$1 AND service_id=$2 AND their_service_id=$3 AND their_device_id=$4`,
			b.dev.ACI, s.serviceID, s.theirServiceID, s.deviceID)
		if err != nil {
			return 0, err
		}
		b.log.Info().Str("peer", s.theirServiceID).Int("device", s.deviceID).Msg("Removed session without current state")
	}
	return len(dead), nil
}

var sessionNotFoundRE = regexp.MustCompile(`session with (\S+)\.(\d+) not found`)

// pruneFromError removes the dead session named in a libsignal
// "session not found" error. Reports whether anything was removed.
func (b *Backend) pruneFromError(ctx context.Context, err error) bool {
	if err == nil {
		return false
	}
	m := sessionNotFoundRE.FindStringSubmatch(err.Error())
	if m == nil {
		return false
	}
	dev, _ := strconv.Atoi(m[2])
	addr, aerr := libsignalgo.NewUUIDAddressFromString(m[1], uint(dev))
	if aerr != nil {
		return false
	}
	rec, lerr := b.dev.ACISessionStore.LoadSession(ctx, addr)
	if lerr != nil || rec == nil {
		return false
	}
	if ok, herr := rec.HasCurrentState(); herr != nil || ok {
		return false
	}
	if rerr := b.dev.ACISessionStore.RemoveSession(ctx, addr); rerr != nil {
		b.log.Err(rerr).Msg("Removing dead session")
		return false
	}
	b.log.Info().Str("peer", m[1]).Int("device", dev).Msg("Removed session without current state; retrying send")
	return true
}
