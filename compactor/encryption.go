package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Parquet encryption of the tiers (compactor/README.md, "Encryption"). The
// keys are files in PARQUET_KEYS_DIR, the file name the key id; new objects
// are encrypted with PARQUET_KEY_ID's key, and an existing object is always
// rewritten under the key its name carries. Sidecars stay plain.

// parquetKeyBytes is the one key size a key file may hold. DuckDB takes
// the base64 text of a 16 or 24 byte key, 24 or 32 characters, as the key
// itself, not decoded (sqlfiles_test.go pins it), so only a 32 byte key's
// text means the same key to every reader.
const parquetKeyBytes = 32

// parquetKeys maps a key id to its key, base64 as the key file holds it.
type parquetKeys map[string]string

// loadParquetKeys reads a keys directory: a mounted Secret, so entries
// starting with a dot (the mount's own) are skipped. Any other entry must
// be a key file: its name a key id, its content the standard base64 of 32
// bytes. Errors name the file, never its content.
func loadParquetKeys(dir string) (parquetKeys, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("PARQUET_KEYS_DIR: %w", err)
	}
	keys := parquetKeys{}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if !keyIDRE.MatchString(name) {
			return nil, fmt.Errorf("PARQUET_KEYS_DIR %s: %q is not a key id (want %s)", dir, name, keyIDRE)
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("PARQUET_KEYS_DIR: key %s: %w", name, err)
		}
		text := strings.TrimSpace(string(b))
		raw, err := base64.StdEncoding.DecodeString(text)
		if err != nil || len(raw) != parquetKeyBytes {
			return nil, fmt.Errorf("PARQUET_KEYS_DIR: key %s: want the standard base64 of %d bytes (openssl rand -base64 %d)",
				name, parquetKeyBytes, parquetKeyBytes)
		}
		keys[name] = text
	}
	return keys, nil
}

// loadKeys reads the keys directory and gives the engine every key that is
// new or changed. Keys live in the database, not a connection, and stay
// there when their file goes: whether a key is missing is the files' say
// (checkKeys), not the engine's.
func (c *compactor) loadKeys(ctx context.Context) error {
	if c.keysDir == "" {
		return nil
	}
	keys, err := loadParquetKeys(c.keysDir)
	if err != nil {
		return err
	}
	conn, err := c.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	for _, id := range sortedIDs(keys) {
		k := keys[id]
		if c.keys[id] == k {
			continue
		}
		if _, err := conn.ExecContext(ctx, "PRAGMA add_parquet_key("+sqlString(id)+", "+sqlString(k)+")"); err != nil {
			return fmt.Errorf("PRAGMA add_parquet_key(%s): %s", id, strings.ReplaceAll(err.Error(), k, "<redacted>"))
		}
		if _, ok := c.keys[id]; ok {
			c.log.Warn("parquet key changed: objects written with its previous content no longer read", "key", id)
		}
	}
	c.keys = keys
	return nil
}

func sortedIDs[V any](m map[string]V) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// keyCheck writes and reads back an object encrypted with the active key
// in the scratch directory: the engine writes encrypted Parquet only with
// httpfs's crypto loaded, and this fails at start, not at the first fold.
func (c *compactor) keyCheck(ctx context.Context, scratch string) error {
	if c.keyID == "" {
		return nil
	}
	f := sqlString(filepath.Join(scratch, "blobs-compactor-key-check.parquet"))
	cfg := encryptionConfig(c.keyID)
	var n int
	if _, err := c.db.ExecContext(ctx, "COPY (SELECT 42 AS x) TO "+f+" (FORMAT parquet, ENCRYPTION_CONFIG "+cfg+")"); err != nil {
		return fmt.Errorf("an encrypted write with PARQUET_KEY_ID %s: %w", c.keyID, err)
	}
	if err := c.db.QueryRowContext(ctx, "SELECT x FROM read_parquet("+f+", encryption_config = "+cfg+")").Scan(&n); err != nil || n != 42 {
		return fmt.Errorf("an encrypted read with PARQUET_KEY_ID %s: %d %v", c.keyID, n, err)
	}
	return nil
}

func encryptionConfig(keyID string) string { return "{footer_key: " + sqlString(keyID) + "}" }

// errKeysMissing is the refusal to fold while a tier object's key is gone.
type errKeysMissing struct{ ids []string }

func (e errKeysMissing) Error() string {
	return fmt.Sprintf("refusing to fold: the tiers hold objects encrypted with parquet keys %s, which PARQUET_KEYS_DIR lacks; "+
		"retention of the tiers must not outlast their keys' (compactor/README.md, \"Encryption\")", strings.Join(e.ids, ", "))
}

// checkKeys reloads the keys directory and lists the key ids that the
// tiers' object names carry, plus the active one. If any has no key file,
// folds and rollups refuse until a check finds them all again, and
// blobs_compactor_parquet_keys_missing says how many are missing. With no
// keys directory every encrypted object's key is missing, so a compactor
// without keys never writes a plain object beside an encrypted one.
func (c *compactor) checkKeys(ctx context.Context) error {
	err := c.findMissingKeys(ctx)
	c.refusal = err
	if err != nil {
		c.log.Error("parquet keys", "err", err)
	}
	return err
}

func (c *compactor) findMissingKeys(ctx context.Context) error {
	if err := c.loadKeys(ctx); err != nil {
		return err
	}
	ids := map[string]bool{}
	if c.keyID != "" {
		ids[c.keyID] = true
	}
	for _, p := range c.l.tierPrefixes() {
		objs, err := c.tiers.List(ctx, p)
		if err != nil {
			return err
		}
		for _, o := range objs {
			if id := tierKeyID(o.Key); id != "" {
				ids[id] = true
			}
		}
	}
	var missing []string
	for _, id := range sortedIDs(ids) {
		if _, ok := c.keys[id]; !ok {
			missing = append(missing, id)
		}
	}
	c.m.keysMissing(len(missing))
	if len(missing) > 0 {
		return errKeysMissing{missing}
	}
	return nil
}

// locate returns stem's object: the one that exists, under whatever key it
// was first written with, or else a new one under the active key. Without
// a keys directory there are no encrypted objects (checkKeys refuses
// otherwise), so the plain name is the only one and a HeadObject tells
// whether it exists, as before encryption.
func (c *compactor) locate(ctx context.Context, stem string) (tierObject, bool, error) {
	if c.keysDir == "" {
		o := tierName(stem, "")
		ok, err := c.tiers.Stat(ctx, o.key)
		return o, ok, err
	}
	objs, err := c.tiers.List(ctx, stem+".")
	if err != nil {
		return tierObject{}, false, err
	}
	var found []tierObject
	for _, o := range objs {
		if t, ok := stemObject(stem, o.Key); ok {
			found = append(found, t)
		}
	}
	switch len(found) {
	case 0:
		return tierName(stem, c.keyID), false, nil
	case 1:
		return found[0], true, nil
	default:
		return tierObject{}, false, fmt.Errorf("%s has %d objects, %v: the compactor keeps one per stem, under its first key", stem, len(found), found)
	}
}

// readTier is a query over objs for a FROM clause, with opts (each ending
// in ", ") for every read. The plain objects are one read_parquet with
// union_by_name; each encrypted object is a read of its own, because one
// read takes one key and union_by_name on an encrypted read is an engine
// error in DuckDB 1.5 (sqlfiles_test.go). UNION ALL BY NAME unites them,
// which also aligns the columns of objects written before a raw1 column
// was added.
func (c *compactor) readTier(objs []tierObject, opts string) string {
	var plain, parts []string
	for _, o := range objs {
		if o.keyID == "" {
			plain = append(plain, c.tiers.URL(o.key))
			continue
		}
		parts = append(parts, "SELECT * FROM read_parquet("+sqlString(c.tiers.URL(o.key))+", "+opts+
			"encryption_config = "+encryptionConfig(o.keyID)+")")
	}
	if len(plain) > 0 {
		parts = append([]string{"SELECT * FROM read_parquet(" + listLiteral(plain) + ", " + opts + "union_by_name = true)"}, parts...)
	}
	return strings.Join(parts, " UNION ALL BY NAME ")
}
