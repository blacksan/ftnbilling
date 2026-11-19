package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// jsonBackend keeps the whole database in DIR/ispbill.json, rewritten
// atomically on every commit. It is the default (no DATABASE_URL).
type jsonBackend struct {
	path string
}

// NewJSONBackend returns the JSON-file backend for dir (created if missing).
func NewJSONBackend(dir string) (Backend, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &jsonBackend{path: filepath.Join(dir, "ispbill.json")}, nil
}

// Open loads (or creates) the JSON database at dir/ispbill.json. It refuses
// to start on a file written by an older, incompatible version instead of
// silently reading it wrongly. Older supported formats are migrated and the
// original is kept next to it as ispbill.json.v<N>.bak.
func Open(dir string) (*Store, error) {
	b, err := NewJSONBackend(dir)
	if err != nil {
		return nil, err
	}
	return OpenWith(b)
}

func (j *jsonBackend) Kind() string { return "json" }
func (j *jsonBackend) Close() error { return nil }

// Path is the database file (tests).
func (j *jsonBackend) Path() string { return j.path }

func (j *jsonBackend) Load(context.Context) (*Data, error) {
	raw, err := os.ReadFile(j.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		d := &Data{}
		d.init()
		return d, nil
	case err != nil:
		return nil, err
	}
	d, from, err := decodeJSON(raw, j.path)
	if err != nil {
		return nil, err
	}
	if from != FormatVersion {
		if err := WriteFileAtomic(fmt.Sprintf("%s.v%d.bak", j.path, from), raw); err != nil {
			return nil, fmt.Errorf("could not back up %s before migrating: %w", j.path, err)
		}
		d.init()
		normalizeData(d)
		if err := j.write(d); err != nil {
			return nil, fmt.Errorf("could not save migrated %s: %w", j.path, err)
		}
	}
	return d, nil
}

func (j *jsonBackend) write(d *Data) error {
	b, err := json.MarshalIndent(d, "", " ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(j.path, b)
}

func (j *jsonBackend) Commit(_ context.Context, d *Data, _ *ChangeSet) error { return j.write(d) }

// ReadJSON reads a JSON database file of format 2 to 8 and returns it in
// the current format, in memory only: nothing is written or backed up. from is
// the format the file had.
func ReadJSON(path string) (d *Data, from int, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	d, from, err = decodeJSON(raw, path)
	if err != nil {
		return nil, 0, err
	}
	d.init()
	normalizeData(d)
	return d, from, nil
}

// WriteJSON writes d as a format 8 JSON database file (export-json).
func WriteJSON(path string, d *Data) error {
	d.Format = FormatVersion
	b, err := json.MarshalIndent(d, "", " ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, b)
}

// decodeJSON parses raw and migrates it in memory to the current format.
func decodeJSON(raw []byte, path string) (*Data, int, error) {
	var probe struct {
		Format int `json:"format"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, 0, fmt.Errorf("corrupt %s: %w", path, err)
	}
	d := &Data{}
	switch probe.Format {
	case FormatVersion, 7, 6, 5, 4, 3, 2:
		if err := json.Unmarshal(raw, d); err != nil {
			return nil, 0, fmt.Errorf("corrupt %s: %w", path, err)
		}
	default:
		return nil, 0, fmt.Errorf("%w: %s has format %d, this version needs %d (old PIN/subscriber model). "+
			"Move or delete the file to start fresh; ispbill will not touch it", ErrOldFormat, path, probe.Format, FormatVersion)
	}
	d.init()
	if probe.Format == 2 {
		d.migrateRows(timeNow())
	}
	if probe.Format < 4 {
		d.migrateV3toV4(timeNow())
	}
	// 4 -> 5 only adds the settings table, which init() has just created empty.
	if probe.Format < 6 {
		d.migrateV5toV6(timeNow())
	}
	if probe.Format < 7 {
		d.migrateV6toV7()
	}
	if probe.Format < 8 {
		d.migrateV7toV8(timeNow())
	}
	d.LegacyContacts = nil
	d.Format = FormatVersion
	return d, probe.Format, nil
}

// WriteFileAtomic writes to a temp file in the same directory, fsyncs, renames
// over path, then fsyncs the directory so the rename survives power loss.
func WriteFileAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".ispbill-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	cleanup := func() { f.Close(); os.Remove(tmp) }
	if _, err := f.Write(b); err != nil {
		cleanup()
		return err
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
	return nil
}
