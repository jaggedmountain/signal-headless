package importer

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"go.mau.fi/mautrix-signal/pkg/libsignalgo"
	"go.mau.fi/mautrix-signal/pkg/signalmeow"

	"signal-headless/internal/db"
)

const cliSchema = `
CREATE TABLE recipient (_id INTEGER PRIMARY KEY, number TEXT, aci TEXT, pni TEXT, profile_key BLOB,
  given_name TEXT, family_name TEXT, nick_name_given_name TEXT, nick_name_family_name TEXT,
  profile_given_name TEXT, profile_family_name TEXT, profile_about TEXT, profile_about_emoji TEXT,
  blocked INTEGER NOT NULL DEFAULT FALSE);
CREATE TABLE pre_key (_id INTEGER PRIMARY KEY, account_id_type INTEGER NOT NULL, key_id INTEGER NOT NULL,
  public_key BLOB NOT NULL, private_key BLOB NOT NULL, stale_timestamp INTEGER);
CREATE TABLE signed_pre_key (_id INTEGER PRIMARY KEY, account_id_type INTEGER NOT NULL, key_id INTEGER NOT NULL,
  public_key BLOB NOT NULL, private_key BLOB NOT NULL, signature BLOB NOT NULL, timestamp INTEGER DEFAULT 0);
CREATE TABLE kyber_pre_key (_id INTEGER PRIMARY KEY, account_id_type INTEGER NOT NULL, key_id INTEGER NOT NULL,
  serialized BLOB NOT NULL, is_last_resort INTEGER NOT NULL, stale_timestamp INTEGER, timestamp INTEGER DEFAULT 0);
CREATE TABLE group_v2 (_id INTEGER PRIMARY KEY, group_id BLOB UNIQUE NOT NULL, master_key BLOB NOT NULL);
CREATE TABLE session (_id INTEGER PRIMARY KEY, account_id_type INTEGER NOT NULL, address TEXT NOT NULL,
  device_id INTEGER NOT NULL, record BLOB NOT NULL);
CREATE TABLE identity (_id INTEGER PRIMARY KEY, address TEXT UNIQUE NOT NULL, identity_key BLOB NOT NULL,
  added_timestamp INTEGER NOT NULL, trust_level INTEGER NOT NULL);
CREATE TABLE sender_key (_id INTEGER PRIMARY KEY, address TEXT NOT NULL, device_id INTEGER NOT NULL,
  distribution_id BLOB NOT NULL, record BLOB NOT NULL, created_timestamp INTEGER NOT NULL);
`

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func genIdentity() (*libsignalgo.IdentityKeyPair, serviceAccountData) {
	kp := must(libsignalgo.GenerateIdentityKeyPair())
	pub := must(kp.GetPublicKey().Serialize())
	priv := must(kp.GetPrivateKey().Serialize())
	return kp, serviceAccountData{
		RegistrationID:     1234,
		IdentityPublicKey:  base64.StdEncoding.EncodeToString(pub),
		IdentityPrivateKey: base64.StdEncoding.EncodeToString(priv),
	}
}

func TestImport(t *testing.T) {
	ctx := context.Background()
	cliDir := t.TempDir()
	dataDir := filepath.Join(cliDir, "data")
	os.MkdirAll(filepath.Join(dataDir, "42.d"), 0o700)

	aci, pni := uuid.New(), uuid.New()
	aciKP, aciData := genIdentity()
	_, pniData := genIdentity()
	aciData.ServiceID = aci.String()
	pniData.ServiceID = "PNI:" + pni.String()
	aep := "abcdefghijklmnopqrstuvwxyz0123456789abcdefghijklmnopqrstuvwxyz01"
	profileKey := make([]byte, 32)
	acct := accountFile{
		Version: 10, ServiceEnvironment: "LIVE", Registered: true, Number: "+15550001111",
		DeviceID: 3, IsMultiDevice: true, Password: "secretpassword",
		ACI: aciData, PNI: pniData, AccountEntropyPool: string(aep),
		ProfileKey: base64.StdEncoding.EncodeToString(profileKey),
	}
	os.WriteFile(filepath.Join(dataDir, "42"), must(json.Marshal(acct)), 0o600)
	os.WriteFile(filepath.Join(dataDir, "accounts.json"),
		[]byte(`{"accounts":[{"path":"42","environment":"LIVE","number":"+15550001111"}],"version":2}`), 0o600)

	src := must(sql.Open("sqlite3", filepath.Join(dataDir, "42.d", "account.db")))
	if _, err := src.Exec(cliSchema); err != nil {
		t.Fatal(err)
	}
	// One-time prekey (ACI) and signed prekey (PNI).
	pk := must(libsignalgo.GeneratePrivateKey())
	src.Exec(`INSERT INTO pre_key (account_id_type, key_id, public_key, private_key) VALUES (0, 11717716, ?, ?)`,
		must(must(pk.GetPublicKey()).Serialize()), must(pk.Serialize()))
	spk := signalmeow.GenerateSignedPreKey(12561043, aciKP)
	spkPub := must(must(spk.GetPublicKey()).Serialize())
	spkPriv := must(must(spk.GetPrivateKey()).Serialize())
	src.Exec(`INSERT INTO signed_pre_key (account_id_type, key_id, public_key, private_key, signature, timestamp) VALUES (1, 12561043, ?, ?, ?, ?)`,
		spkPub, spkPriv, must(spk.GetSignature()), time.Now().UnixMilli())
	for i, k := range signalmeow.GenerateKyberPreKeys(500, 2, aciKP) {
		src.Exec(`INSERT INTO kyber_pre_key (account_id_type, key_id, serialized, is_last_resort) VALUES (0, ?, ?, ?)`,
			500+i, must(k.Serialize()), i == 1)
	}
	bob, bobPNI := uuid.New(), uuid.New()
	bobKey := must(libsignalgo.GenerateIdentityKeyPair())
	src.Exec(`INSERT INTO identity (address, identity_key, added_timestamp, trust_level) VALUES (?, ?, 0, 1)`,
		bob.String(), must(bobKey.GetIdentityKey().Serialize()))
	src.Exec(`INSERT INTO recipient (number, aci, pni, profile_key, given_name, family_name, nick_name_given_name, profile_given_name)
		VALUES ('+15550002222', ?, ?, ?, 'Bob', 'Builder', '', 'Bobby')`, bob.String(), bobPNI.String(), profileKey)
	src.Exec(`INSERT INTO recipient (pni) VALUES (?)`, "PNI:"+uuid.New().String())
	src.Exec(`INSERT INTO recipient (number) VALUES ('+15550003333')`) // unaddressable: skipped
	gid, mk := make([]byte, 32), make([]byte, 32)
	gid[0], mk[0] = 1, 2
	src.Exec(`INSERT INTO group_v2 (group_id, master_key) VALUES (?, ?)`, gid, mk)
	src.Close()

	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "sh.db"), zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rep, err := Import(ctx, d, cliDir, "", zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	want := Report{Number: "+15550001111", DeviceID: 3, Identities: 1, PreKeys: 1, SignedPreKeys: 1, KyberPreKeys: 2, Recipients: 2, Groups: 1}
	if *rep != want {
		t.Fatalf("report = %+v\nwant     %+v", *rep, want)
	}

	dev, err := d.Signal.DeviceByACI(ctx, aci)
	if err != nil || dev == nil {
		t.Fatalf("device: %v", err)
	}
	if dev.DeviceID != 3 || dev.Password != "secretpassword" || dev.PNI != pni || dev.MasterKey == nil {
		t.Fatalf("device data = %+v", dev.DeviceData)
	}
	if ok, _ := dev.ACIIdentityKeyPair.GetPublicKey().Equal(aciKP.GetPublicKey()); !ok {
		t.Fatal("ACI identity key mismatch")
	}
	if _, err := dev.ACIPreKeyStore.LoadPreKey(ctx, 11717716); err != nil {
		t.Fatalf("prekey: %v", err)
	}
	if _, err := dev.PNIPreKeyStore.LoadSignedPreKey(ctx, 12561043); err != nil {
		t.Fatalf("signed prekey: %v", err)
	}
	if last, err := dev.ACIPreKeyStore.IsKyberPreKeyLastResort(ctx, 501); err != nil || !last {
		t.Fatalf("kyber last resort: %v %v", last, err)
	}
	r, err := dev.RecipientStore.LoadAndUpdateRecipient(ctx, bob, uuid.Nil, nil)
	if err != nil || r.ContactName != "Bob Builder" || r.Profile.Name != "Bobby" || r.E164 != "+15550002222" {
		t.Fatalf("recipient = %+v err=%v", r, err)
	}
	k, err := dev.IdentityKeyStore.GetIdentityKey(ctx, libsignalgo.NewACIServiceID(bob))
	if err != nil || k == nil {
		t.Fatalf("identity: %v", err)
	}
	if _, err := Import(ctx, d, cliDir, "", zerolog.Nop()); err == nil {
		t.Fatal("second import should refuse")
	}
}
