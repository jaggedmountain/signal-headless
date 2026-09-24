// Package importer adopts a device already linked by signal-cli, so the same
// Signal identity continues here without re-linking.
//
// It only ever reads signal-cli's files. After a successful import signal-cli
// must not be run for that account again: both would advance the same
// ratchets and break each other's sessions.
package importer

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"go.mau.fi/mautrix-signal/pkg/libsignalgo"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/store"
	"go.mau.fi/mautrix-signal/pkg/signalmeow/types"

	"signal-headless/internal/db"
)

type Report struct {
	Number        string
	DeviceID      int
	Identities    int
	Sessions      int
	PreKeys       int
	SignedPreKeys int
	KyberPreKeys  int
	SenderKeys    int
	Recipients    int
	Groups        int
}

func (r Report) String() string {
	return fmt.Sprintf("account %s device %d: %d identities, %d sessions, %d prekeys, %d signed prekeys, %d kyber prekeys, %d sender keys, %d recipients, %d groups",
		r.Number, r.DeviceID, r.Identities, r.Sessions, r.PreKeys, r.SignedPreKeys, r.KyberPreKeys, r.SenderKeys, r.Recipients, r.Groups)
}

type accountsFile struct {
	Accounts []struct {
		Path        string `json:"path"`
		Environment string `json:"environment"`
		Number      string `json:"number"`
		UUID        string `json:"uuid"`
	} `json:"accounts"`
}

type serviceAccountData struct {
	ServiceID          string `json:"serviceId"`
	RegistrationID     int    `json:"registrationId"`
	IdentityPrivateKey string `json:"identityPrivateKey"`
	IdentityPublicKey  string `json:"identityPublicKey"`
}

type accountFile struct {
	Version            int                `json:"version"`
	ServiceEnvironment string             `json:"serviceEnvironment"`
	Registered         bool               `json:"registered"`
	Number             string             `json:"number"`
	DeviceID           int                `json:"deviceId"`
	IsMultiDevice      bool               `json:"isMultiDevice"`
	Password           string             `json:"password"`
	ACI                serviceAccountData `json:"aciAccountData"`
	PNI                serviceAccountData `json:"pniAccountData"`
	AccountEntropyPool string             `json:"accountEntropyPool"`
	MediaRootBackupKey string             `json:"mediaRootBackupKey"`
	ProfileKey         string             `json:"profileKey"`
}

// locate finds the account file and database for number ("" = the only account).
func locate(cliDir, number string) (jsonPath, dbPath string, err error) {
	dataDir := filepath.Join(cliDir, "data")
	raw, err := os.ReadFile(filepath.Join(dataDir, "accounts.json"))
	if err != nil {
		return "", "", fmt.Errorf("read signal-cli accounts: %w", err)
	}
	var af accountsFile
	if err := json.Unmarshal(raw, &af); err != nil {
		return "", "", fmt.Errorf("parse accounts.json: %w", err)
	}
	var matches []string
	for _, a := range af.Accounts {
		if a.Environment != "" && a.Environment != "LIVE" {
			continue
		}
		if number == "" || a.Number == number {
			matches = append(matches, a.Path)
		}
	}
	switch {
	case len(matches) == 0:
		return "", "", fmt.Errorf("no matching signal-cli account in %s", dataDir)
	case len(matches) > 1:
		return "", "", errors.New("several signal-cli accounts; choose one with --account")
	}
	return filepath.Join(dataDir, matches[0]), filepath.Join(dataDir, matches[0]+".d", "account.db"), nil
}

// SignalCLIRunning reports running signal-cli processes (by /proc cmdline).
func SignalCLIRunning() []int {
	var pids []int
	entries, _ := os.ReadDir("/proc")
	self := os.Getpid()
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		cmd, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil || len(cmd) == 0 {
			continue
		}
		argv0 := string(cmd)
		if i := strings.IndexByte(argv0, 0); i >= 0 {
			argv0 = argv0[:i]
		}
		if filepath.Base(argv0) == "signal-cli" || strings.Contains(string(cmd), "org.asamk.signal.Main") {
			pids = append(pids, pid)
		}
	}
	return pids
}

func b64(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.RawStdEncoding.DecodeString(s)
}

func keyPair(sd serviceAccountData) (*libsignalgo.IdentityKeyPair, error) {
	pubB, err := b64(sd.IdentityPublicKey)
	if err != nil {
		return nil, err
	}
	privB, err := b64(sd.IdentityPrivateKey)
	if err != nil {
		return nil, err
	}
	pub, err := libsignalgo.DeserializePublicKey(pubB)
	if err != nil {
		return nil, fmt.Errorf("public key: %w", err)
	}
	priv, err := libsignalgo.DeserializePrivateKey(privB)
	if err != nil {
		return nil, fmt.Errorf("private key: %w", err)
	}
	return libsignalgo.NewIdentityKeyPair(pub, priv)
}

// Import copies the signal-cli account into d. d must not hold a device yet.
func Import(ctx context.Context, d *db.DB, cliDir, number string, log zerolog.Logger) (*Report, error) {
	jsonPath, dbPath, err := locate(cliDir, number)
	if err != nil {
		return nil, err
	}
	if existing, err := d.Signal.GetAllDevices(ctx); err != nil {
		return nil, err
	} else if len(existing) > 0 {
		return nil, fmt.Errorf("a device (%s) is already stored; refusing to import over it", existing[0].Number)
	}
	raw, err := os.ReadFile(jsonPath)
	if err != nil {
		return nil, err
	}
	var af accountFile
	if err := json.Unmarshal(raw, &af); err != nil {
		return nil, fmt.Errorf("parse account file: %w", err)
	}
	if !af.Registered || af.Password == "" || af.DeviceID == 0 {
		return nil, errors.New("signal-cli account is not a registered/linked device")
	}
	if af.ServiceEnvironment != "" && af.ServiceEnvironment != "LIVE" {
		return nil, fmt.Errorf("unsupported environment %q", af.ServiceEnvironment)
	}
	aci, err := uuid.Parse(af.ACI.ServiceID)
	if err != nil {
		return nil, fmt.Errorf("ACI: %w", err)
	}
	pni, err := uuid.Parse(strings.TrimPrefix(af.PNI.ServiceID, "PNI:"))
	if err != nil {
		return nil, fmt.Errorf("PNI: %w", err)
	}
	aciKeys, err := keyPair(af.ACI)
	if err != nil {
		return nil, fmt.Errorf("ACI identity: %w", err)
	}
	pniKeys, err := keyPair(af.PNI)
	if err != nil {
		return nil, fmt.Errorf("PNI identity: %w", err)
	}
	dd := &store.DeviceData{
		ACIIdentityKeyPair: aciKeys,
		PNIIdentityKeyPair: pniKeys,
		ACIRegistrationID:  af.ACI.RegistrationID,
		PNIRegistrationID:  af.PNI.RegistrationID,
		ACI:                aci,
		PNI:                pni,
		DeviceID:           af.DeviceID,
		Number:             af.Number,
		Password:           af.Password,
		AccountEntropyPool: libsignalgo.AccountEntropyPool(af.AccountEntropyPool),
	}
	if af.AccountEntropyPool != "" {
		if dd.MasterKey, err = dd.AccountEntropyPool.DeriveSVRKey(); err != nil {
			return nil, fmt.Errorf("derive master key: %w", err)
		}
	}
	if mrbk, err := b64(af.MediaRootBackupKey); err == nil && len(mrbk) > 0 {
		dd.MediaRootBackupKey = libsignalgo.BytesToBackupKey(mrbk)
	}
	profileKey, err := b64(af.ProfileKey)
	if err != nil || (len(profileKey) != 0 && len(profileKey) != libsignalgo.ProfileKeyLength) {
		return nil, fmt.Errorf("bad profile key")
	}

	src, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer src.Close()
	if err := src.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("open %s: %w", dbPath, err)
	}

	rep := &Report{Number: af.Number, DeviceID: af.DeviceID}
	err = d.DoTxn(ctx, nil, func(ctx context.Context) error {
		if err := d.Signal.PutDevice(ctx, dd); err != nil {
			return fmt.Errorf("store device: %w", err)
		}
		dev, err := d.Signal.DeviceByACI(ctx, aci)
		if err != nil || dev == nil {
			return fmt.Errorf("reload device: %v", err)
		}
		if _, err := dev.IdentityKeyStore.SaveIdentityKey(ctx, dev.ACIServiceID(), aciKeys.GetIdentityKey()); err != nil {
			return err
		}
		if _, err := dev.IdentityKeyStore.SaveIdentityKey(ctx, dev.PNIServiceID(), pniKeys.GetIdentityKey()); err != nil {
			return err
		}
		self := &types.Recipient{ACI: aci, PNI: pni, E164: af.Number}
		if len(profileKey) == libsignalgo.ProfileKeyLength {
			self.Profile.Key = libsignalgo.ProfileKey(profileKey)
		}
		if err := dev.RecipientStore.StoreRecipient(ctx, self); err != nil {
			return err
		}
		steps := []struct {
			name string
			fn   func(context.Context, *sql.DB, *store.Device, *Report) error
		}{
			{"identities", importIdentities},
			{"prekeys", importPreKeys},
			{"signed prekeys", importSignedPreKeys},
			{"kyber prekeys", importKyberPreKeys},
			{"sessions", importSessions},
			{"sender keys", importSenderKeys},
			{"recipients", importRecipients},
			{"groups", importGroups},
		}
		for _, s := range steps {
			if err := s.fn(ctx, src, dev, rep); err != nil {
				return fmt.Errorf("%s: %w", s.name, err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	log.Info().Str("report", rep.String()).Msg("Imported signal-cli device")
	return rep, nil
}

func preKeyStore(dev *store.Device, accountIDType int) store.PreKeyStore {
	if accountIDType == 1 {
		return dev.PNIPreKeyStore
	}
	return dev.ACIPreKeyStore
}

func sessionStore(dev *store.Device, accountIDType int) store.SessionStore {
	if accountIDType == 1 {
		return dev.PNISessionStore
	}
	return dev.ACISessionStore
}

func eachRow(ctx context.Context, src *sql.DB, query string, fn func(*sql.Rows) error) error {
	rows, err := src.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func importIdentities(ctx context.Context, src *sql.DB, dev *store.Device, rep *Report) error {
	return eachRow(ctx, src, `SELECT address, identity_key FROM identity`, func(r *sql.Rows) error {
		var addr string
		var key []byte
		if err := r.Scan(&addr, &key); err != nil {
			return err
		}
		sid, err := libsignalgo.ServiceIDFromString(addr)
		if err != nil {
			return nil // number-only identities can't be addressed; skip
		}
		ik, err := libsignalgo.DeserializeIdentityKey(key)
		if err != nil {
			return fmt.Errorf("identity %s: %w", addr, err)
		}
		if _, err := dev.IdentityKeyStore.SaveIdentityKey(ctx, sid, ik); err != nil {
			return err
		}
		rep.Identities++
		return nil
	})
}

func importPreKeys(ctx context.Context, src *sql.DB, dev *store.Device, rep *Report) error {
	return eachRow(ctx, src, `SELECT account_id_type, key_id, public_key, private_key FROM pre_key`, func(r *sql.Rows) error {
		var typ int
		var id uint32
		var pubB, privB []byte
		if err := r.Scan(&typ, &id, &pubB, &privB); err != nil {
			return err
		}
		pub, err := libsignalgo.DeserializePublicKey(pubB)
		if err != nil {
			return err
		}
		priv, err := libsignalgo.DeserializePrivateKey(privB)
		if err != nil {
			return err
		}
		rec, err := libsignalgo.NewPreKeyRecord(id, pub, priv)
		if err != nil {
			return err
		}
		if err := preKeyStore(dev, typ).StorePreKey(ctx, id, rec); err != nil {
			return err
		}
		rep.PreKeys++
		return nil
	})
}

func importSignedPreKeys(ctx context.Context, src *sql.DB, dev *store.Device, rep *Report) error {
	return eachRow(ctx, src, `SELECT account_id_type, key_id, public_key, private_key, signature, COALESCE(timestamp, 0) FROM signed_pre_key`, func(r *sql.Rows) error {
		var typ int
		var id uint32
		var ts int64
		var pubB, privB, sig []byte
		if err := r.Scan(&typ, &id, &pubB, &privB, &sig, &ts); err != nil {
			return err
		}
		pub, err := libsignalgo.DeserializePublicKey(pubB)
		if err != nil {
			return err
		}
		priv, err := libsignalgo.DeserializePrivateKey(privB)
		if err != nil {
			return err
		}
		rec, err := libsignalgo.NewSignedPreKeyRecord(id, time.UnixMilli(ts), pub, priv, sig)
		if err != nil {
			return err
		}
		if err := preKeyStore(dev, typ).StoreSignedPreKey(ctx, id, rec); err != nil {
			return err
		}
		rep.SignedPreKeys++
		return nil
	})
}

func importKyberPreKeys(ctx context.Context, src *sql.DB, dev *store.Device, rep *Report) error {
	return eachRow(ctx, src, `SELECT account_id_type, key_id, serialized, is_last_resort FROM kyber_pre_key`, func(r *sql.Rows) error {
		var typ int
		var id uint32
		var ser []byte
		var lastResort bool
		if err := r.Scan(&typ, &id, &ser, &lastResort); err != nil {
			return err
		}
		rec, err := libsignalgo.DeserializeKyberPreKeyRecord(ser)
		if err != nil {
			return err
		}
		ks := preKeyStore(dev, typ)
		if lastResort {
			err = ks.StoreLastResortKyberPreKey(ctx, id, rec)
		} else {
			err = ks.StoreKyberPreKey(ctx, id, rec)
		}
		if err != nil {
			return err
		}
		rep.KyberPreKeys++
		return nil
	})
}

func importSessions(ctx context.Context, src *sql.DB, dev *store.Device, rep *Report) error {
	return eachRow(ctx, src, `SELECT account_id_type, address, device_id, record FROM session`, func(r *sql.Rows) error {
		var typ int
		var addr string
		var deviceID uint
		var ser []byte
		if err := r.Scan(&typ, &addr, &deviceID, &ser); err != nil {
			return err
		}
		a, err := libsignalgo.NewUUIDAddressFromString(addr, deviceID)
		if err != nil {
			return nil // unaddressable (number-only) session; skip
		}
		rec, err := libsignalgo.DeserializeSessionRecord(ser)
		if err != nil {
			return fmt.Errorf("session %s.%d: %w", addr, deviceID, err)
		}
		if err := sessionStore(dev, typ).StoreSession(ctx, a, rec); err != nil {
			return err
		}
		rep.Sessions++
		return nil
	})
}

func importSenderKeys(ctx context.Context, src *sql.DB, dev *store.Device, rep *Report) error {
	return eachRow(ctx, src, `SELECT address, device_id, distribution_id, record FROM sender_key`, func(r *sql.Rows) error {
		var addr string
		var deviceID uint
		var distB, ser []byte
		if err := r.Scan(&addr, &deviceID, &distB, &ser); err != nil {
			return err
		}
		a, err := libsignalgo.NewUUIDAddressFromString(addr, deviceID)
		if err != nil {
			return nil
		}
		dist, err := uuid.FromBytes(distB)
		if err != nil {
			return fmt.Errorf("distribution id: %w", err)
		}
		rec, err := libsignalgo.DeserializeSenderKeyRecord(ser)
		if err != nil {
			return err
		}
		if err := dev.SenderKeyStore.StoreSenderKey(ctx, a, dist, rec); err != nil {
			return err
		}
		rep.SenderKeys++
		return nil
	})
}

func joinName(parts ...sql.NullString) string {
	var out []string
	for _, p := range parts {
		if s := strings.TrimSpace(p.String); s != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, " ")
}

func importRecipients(ctx context.Context, src *sql.DB, dev *store.Device, rep *Report) error {
	return eachRow(ctx, src, `
		SELECT aci, pni, number, profile_key, given_name, family_name, nick_name_given_name, nick_name_family_name,
			profile_given_name, profile_family_name, profile_about, profile_about_emoji, blocked
		FROM recipient`, func(r *sql.Rows) error {
		var aciS, pniS, number, given, family, nickGiven, nickFamily, pGiven, pFamily, about, aboutEmoji sql.NullString
		var profileKey []byte
		var blocked bool
		if err := r.Scan(&aciS, &pniS, &number, &profileKey, &given, &family, &nickGiven, &nickFamily,
			&pGiven, &pFamily, &about, &aboutEmoji, &blocked); err != nil {
			return err
		}
		rc := &types.Recipient{
			E164:        number.String,
			ContactName: joinName(given, family),
			Nickname:    joinName(nickGiven, nickFamily),
			Blocked:     blocked,
			Profile: types.Profile{
				Name:       joinName(pGiven, pFamily),
				About:      about.String,
				AboutEmoji: aboutEmoji.String,
			},
		}
		if aciS.Valid {
			rc.ACI, _ = uuid.Parse(aciS.String)
		}
		if pniS.Valid {
			rc.PNI, _ = uuid.Parse(strings.TrimPrefix(pniS.String, "PNI:"))
		}
		if rc.ACI == uuid.Nil && rc.PNI == uuid.Nil {
			return nil
		}
		if rc.ACI == dev.ACI {
			return nil // self was stored from the account file
		}
		if len(profileKey) == libsignalgo.ProfileKeyLength {
			rc.Profile.Key = libsignalgo.ProfileKey(profileKey)
		}
		if err := dev.RecipientStore.StoreRecipient(ctx, rc); err != nil {
			return err
		}
		rep.Recipients++
		return nil
	})
}

func importGroups(ctx context.Context, src *sql.DB, dev *store.Device, rep *Report) error {
	return eachRow(ctx, src, `SELECT group_id, master_key FROM group_v2`, func(r *sql.Rows) error {
		var gidB, mk []byte
		if err := r.Scan(&gidB, &mk); err != nil {
			return err
		}
		if len(gidB) != 32 || len(mk) != 32 {
			return nil
		}
		gid := types.GroupIdentifier(base64.StdEncoding.EncodeToString(gidB))
		key := types.SerializedGroupMasterKey(base64.StdEncoding.EncodeToString(mk))
		if err := dev.GroupStore.StoreMasterKey(ctx, gid, key); err != nil {
			return err
		}
		rep.Groups++
		return nil
	})
}
