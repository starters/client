package libkb

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"golang.org/x/crypto/openpgp"
)

type KeyringFile struct {
	filename         string
	Entities         openpgp.EntityList
	isPublic         bool
	indexId          map[string](*openpgp.Entity) // Map of 64-bit uppercase-hex KeyIds
	indexFingerprint map[PgpFingerprint](*openpgp.Entity)
}

type Keyrings struct {
	Public []*KeyringFile
	Secret []*KeyringFile
	skbMap map[string]*SKBKeyringFile
	sync.Mutex
}

func (k Keyrings) MakeKeyrings(filenames []string, isPublic bool) []*KeyringFile {
	v := make([]*KeyringFile, len(filenames), len(filenames))
	for i, filename := range filenames {
		v[i] = &KeyringFile{filename, openpgp.EntityList{}, isPublic, nil, nil}
	}
	return v
}

func NewKeyrings(e Env, usage Usage) *Keyrings {
	ret := &Keyrings{
		skbMap: make(map[string]*SKBKeyringFile),
	}
	if usage.GpgKeyring {
		ret.Public = ret.MakeKeyrings(e.GetPublicKeyrings(), true)
		ret.Secret = ret.MakeKeyrings(e.GetPgpSecretKeyrings(), false)
	}
	return ret
}

//===================================================================
//
// Make our Keryings struct meet the openpgp.KeyRing
// interface
//

func (k Keyrings) KeysById(id uint64) []openpgp.Key {
	out := make([]openpgp.Key, 10)
	for _, ring := range k.Public {
		out = append(out, ring.Entities.KeysById(id)...)
	}
	for _, ring := range k.Secret {
		out = append(out, ring.Entities.KeysById(id)...)
	}
	return out
}

func (k Keyrings) KeysByIdUsage(id uint64, usage byte) []openpgp.Key {
	out := make([]openpgp.Key, 10)
	for _, ring := range k.Public {
		out = append(out, ring.Entities.KeysByIdUsage(id, usage)...)
	}
	for _, ring := range k.Secret {
		out = append(out, ring.Entities.KeysByIdUsage(id, usage)...)
	}
	return out
}

func (k Keyrings) DecryptionKeys() []openpgp.Key {
	out := make([]openpgp.Key, 10)
	for _, ring := range k.Secret {
		out = append(out, ring.Entities.DecryptionKeys()...)
	}
	return out
}

//===================================================================

func (k Keyrings) FindKey(fp PgpFingerprint, secret bool) *openpgp.Entity {
	var l []*KeyringFile
	if secret {
		l = k.Secret
	} else {
		l = k.Public
	}
	for _, file := range l {
		key, found := file.indexFingerprint[fp]
		if found && key != nil && (!secret || key.PrivateKey != nil) {
			return key
		}
	}

	return nil
}

//===================================================================

func (k *Keyrings) Load() (err error) {
	G.Log.Debug("+ Loading keyrings")
	if k.Public != nil {
		err = k.LoadKeyrings(k.Public)
	}
	if err == nil && k.Secret != nil {
		k.LoadKeyrings(k.Secret)
	}
	G.Log.Debug("- Loaded keyrings")
	return err
}

func (k *Keyrings) LoadKeyrings(v []*KeyringFile) (err error) {
	for _, k := range v {
		if err = k.LoadAndIndex(); err != nil {
			return err
		}
	}
	return nil
}

func SKBFilenameForUser(un string) string {
	tmp := G.Env.GetSecretKeyringTemplate()
	token := "%u"
	if strings.Index(tmp, token) < 0 {
		return tmp
	} else {
		return strings.Replace(tmp, token, un, -1)
	}
}

func (k *Keyrings) LoadSKBKeyring(un string) (f *SKBKeyringFile, err error) {
	k.Lock()
	defer k.Unlock()

	if f = k.skbMap[un]; f != nil {
	} else if len(un) == 0 {
		err = NoUsernameError{}
	} else {
		f = NewSKBKeyringFile(SKBFilenameForUser(un))
		if err = f.LoadAndIndex(); err == nil || os.IsNotExist(err) {
			err = nil
			k.skbMap[un] = f
		}
	}
	return
}

func (k *KeyringFile) LoadAndIndex() error {
	var err error
	G.Log.Debug("+ LoadAndIndex on %s", k.filename)
	if err = k.Load(); err == nil {
		err = k.Index()
	}
	G.Log.Debug("- LoadAndIndex on %s -> %s", k.filename, ErrToOk(err))
	return err
}

func (k *KeyringFile) Index() error {
	G.Log.Debug("+ Index on %s", k.filename)
	k.indexId = make(map[string](*openpgp.Entity))
	k.indexFingerprint = make(map[PgpFingerprint](*openpgp.Entity))
	p := 0
	s := 0
	for _, entity := range k.Entities {
		if entity.PrimaryKey != nil {
			id := entity.PrimaryKey.KeyIdString()
			k.indexId[id] = entity
			fp := PgpFingerprint(entity.PrimaryKey.Fingerprint)
			k.indexFingerprint[fp] = entity
			p++
		}
		for _, subkey := range entity.Subkeys {
			if subkey.PublicKey != nil {
				id := subkey.PublicKey.KeyIdString()
				k.indexId[id] = entity
				fp := PgpFingerprint(subkey.PublicKey.Fingerprint)
				k.indexFingerprint[fp] = entity
				s++
			}
		}
	}
	G.Log.Debug("| Indexed %d primary and %d subkeys", p, s)
	G.Log.Debug("- Index on %s -> %s", k.filename, "OK")
	return nil
}

func (k *KeyringFile) Load() error {
	G.Log.Debug(fmt.Sprintf("+ Loading PGP Keyring %s", k.filename))
	file, err := os.Open(k.filename)
	if os.IsNotExist(err) {
		G.Log.Warning(fmt.Sprintf("No PGP Keyring found at %s", k.filename))
		err = nil
	} else if err != nil {
		G.Log.Error(fmt.Sprintf("Cannot open keyring %s: %s\n", err.Error()))
		return err
	}
	if file != nil {
		k.Entities, err = openpgp.ReadKeyRing(file)
		if err != nil {
			G.Log.Error(fmt.Sprintf("Cannot parse keyring %s: %s\n", err.Error()))
			return err
		}
	}
	G.Log.Debug(fmt.Sprintf("- Successfully loaded PGP Keyring"))
	return nil
}

func (k KeyringFile) WriteTo(w io.Writer) error {
	for _, e := range k.Entities {
		if err := e.Serialize(w); err != nil {
			return err
		}
	}
	return nil
}

func (k KeyringFile) GetFilename() string { return k.filename }

func (k KeyringFile) Save() error {
	return SafeWriteToFile(k)
}

// GetSecretKeyLocked gets a secret key for the current user by first
// looking for keys synced from the server, and if that fails, tries
// those in the local Keyring that are also active for the user.
// In any case, the key will be locked.
func (k Keyrings) GetSecretKeyLocked(ska SecretKeyArg) (ret *SKB, which string, err error) {

	G.Log.Debug("+ GetSecretKeyLocked()")
	defer func() {
		G.Log.Debug("- GetSecretKeyLocked() -> %s", ErrToOk(err))
	}()

	G.Log.Debug("| LoadMe w/ Secrets on")

	if ska.Me == nil {
		if ska.Me, err = LoadMe(LoadUserArg{}); err != nil {
			return
		}
	}

	if ret = k.GetLockedLocalSecretKey(ska); ret != nil {
		G.Log.Debug("| Getting local secret key")
		return
	}

	if !ska.UseSyncedPGPKey() {
		G.Log.Debug("| Skipped Synced PGP key (via prefs")
	} else if ret, err = ska.Me.GetSyncedSecretKey(); err != nil {
		G.Log.Warning("Error fetching synced PGP secret key: %s", err.Error())
		return
	} else if ret != nil {
		which = "your Keybase.io passphrase"
	}

	if ret == nil {
		err = NoSecretKeyError{}
	}
	return

}

// GetLockedLocalSecretKey looks in the local keyring to find a key
// for the given user.  Return non-nil if one was found, and nil
// otherwise.
func (k Keyrings) GetLockedLocalSecretKey(ska SecretKeyArg) (ret *SKB) {
	var keyring *SKBKeyringFile
	var err error
	var ckf *ComputedKeyFamily

	me := ska.Me

	G.Log.Debug("+ GetLockedLocalSecretKey(%s)", me.name)
	defer func() {
		G.Log.Debug("- GetLockedLocalSecretKey -> found=%v", ret != nil)
	}()

	if keyring, err = k.LoadSKBKeyring(me.name); err != nil || keyring == nil {
		var s string
		if err != nil {
			s = " (" + err.Error() + ")"
		}
		G.Log.Debug("| No secret keyring found" + s)
		return
	}

	if ckf = me.GetComputedKeyFamily(); ckf == nil {
		G.Log.Warning("No ComputedKeyFamily found for %s", me.name)
		return
	}

	var kid KID
	if !ska.UseDeviceKey() {
		G.Log.Debug("| not using device key; preferences have disabled it")
	} else if kid, err = ckf.GetActiveSibkeyKidForCurrentDevice(); err != nil {
		G.Log.Debug("| No key for current device: %s", err.Error())
	} else if kid != nil {
		G.Log.Debug("| Found KID for current device: %s", kid)
		ret = keyring.LookupByKid(kid)
		if ret != nil {
			G.Log.Debug("| Using device key: %s", kid)
		}
	} else {
		G.Log.Debug("| Empty kid for current device")
	}

	if ret == nil && ska.SearchForKey() {
		G.Log.Debug("| Looking up secret key in local keychain")
		ret = keyring.SearchWithComputedKeyFamily(ckf)
	}
	return ret
}

type SecretKeyArg struct {

	// Which keys to search for
	All          bool // use all possible keys
	DeviceKey    bool // use the device key (on by default)
	SyncedPGPKey bool // use the sync'ed PGP key if there is one
	SearchKey    bool // search for any key that's active in the local keyring

	Reason string   // why it's needed (for an Unlock)
	Ui     SecretUI // for Unlocking secrets
	Me     *User    // Whose keys
}

func (s SecretKeyArg) UseDeviceKey() bool    { return s.All || s.DeviceKey }
func (s SecretKeyArg) SearchForKey() bool    { return s.All || s.SearchKey }
func (s SecretKeyArg) UseSyncedPGPKey() bool { return s.All || s.SyncedPGPKey }

func (k Keyrings) GetSecretKey(ska SecretKeyArg) (key GenericKey, err error) {
	G.Log.Debug("+ GetSecretKey(%s)", ska.Reason)
	defer func() {
		G.Log.Debug("- GetSecretKey() -> %s", ErrToOk(err))
	}()
	var skb *SKB
	var which string
	if skb, which, err = k.GetSecretKeyLocked(ska); err == nil && skb != nil {
		G.Log.Debug("| Prompt/Unlock key")
		key, err = skb.PromptAndUnlock(ska.Reason, which, ska.Ui)
	}
	return
}

type EmptyKeyRing struct{}

func (k EmptyKeyRing) KeysById(id uint64) []openpgp.Key {
	return []openpgp.Key{}
}
func (k EmptyKeyRing) KeysByIdUsage(id uint64, usage byte) []openpgp.Key {
	return []openpgp.Key{}
}
func (k EmptyKeyRing) DecryptionKeys() []openpgp.Key {
	return []openpgp.Key{}
}

func (g *Global) LoadSKBKeyring(name string) (f *SKBKeyringFile, err error) {
	if g.Keyrings == nil {
		err = NoKeyringsError{}
	} else {
		f, err = g.Keyrings.LoadSKBKeyring(name)
	}
	return
}
