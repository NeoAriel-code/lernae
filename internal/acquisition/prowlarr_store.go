package acquisition

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// These are private disk documents, not common-domain fields or a response
// cache. Only the minimum exact snapshot is persisted, never upstream JSON.
type prowlarrLocator struct {
	Route string
	Link  string
	File  string
}

func (prowlarrLocator) String() string     { return "prowlarrLocator{values:<private>}" }
func (l prowlarrLocator) GoString() string { return l.String() }

type prowlarrRecord struct {
	Version   int
	Instance  string
	IndexerID int64
	GUID      string
	Protocol  ProwlarrProtocol
	Title     string
	HasSize   bool
	Size      int64
	Download  prowlarrLocator
	Magnet    prowlarrLocator
}

func (prowlarrRecord) String() string     { return "prowlarrRecord{snapshot:<private>}" }
func (r prowlarrRecord) GoString() string { return r.String() }
func (r prowlarrRecord) bytes() []byte {
	data, _ := json.Marshal(r)
	return data
}
func (r prowlarrRecord) reference() string { return prowlarrHash("reference-v1", r.bytes()) }
func (r prowlarrRecord) candidate() string {
	// Length-delimited canonical tuple prevents GUID/path delimiter ambiguity.
	data, _ := json.Marshal(struct {
		Instance  string
		IndexerID int64
		GUID      string
	}{r.Instance, r.IndexerID, r.GUID})
	return prowlarrHash("candidate-v1", data)
}
func (r prowlarrRecord) metadata() Metadata {
	return Metadata{Title: r.Title, Label: prowlarrLabel(r.HasSize, r.Size)}
}

// ProwlarrLocator exposes keyless typed private state only to trusted internal
// future consumers. It never navigates a route, decrypts a link or grabs a release.
type ProwlarrLocator struct {
	Route string `json:"-"`
	Link  string `json:"-"`
	File  string `json:"-"`
}

func (ProwlarrLocator) String() string     { return "ProwlarrLocator{values:<private>}" }
func (l ProwlarrLocator) GoString() string { return l.String() }

type ProwlarrRecord struct {
	InstanceHash string           `json:"-"`
	CandidateID  string           `json:"-"`
	IndexerID    int64            `json:"-"`
	GUID         string           `json:"-"`
	Protocol     ProwlarrProtocol `json:"-"`
	Metadata     Metadata         `json:"-"`
	HasSize      bool             `json:"-"`
	Size         int64            `json:"-"`
	Download     ProwlarrLocator  `json:"-"`
	Magnet       ProwlarrLocator  `json:"-"`
}

func (ProwlarrRecord) String() string     { return "ProwlarrRecord{snapshot:<private>}" }
func (r ProwlarrRecord) GoString() string { return r.String() }

// HasProxyLocator is only availability of captured private transport metadata,
// not a promise of executable effects or protection-key continuity.
func (r ProwlarrRecord) HasProxyLocator() bool { return r.Download.Link != "" || r.Magnet.Link != "" }

// Lookup directly reopens EXACT immutable state after restart. Missing, changed,
// unsafe or differently instance-scoped records fail closed, without Search.
// API-key rotation does not rewrite records; the key is not persisted here.
func (p *Prowlarr) Lookup(ctx context.Context, token string) (ProwlarrRecord, error) {
	if safe := prowlarrContextError(ctx); safe != nil {
		return ProwlarrRecord{}, safe
	}
	if p == nil || p.base == nil || !handlePattern.MatchString(token) {
		return ProwlarrRecord{}, ErrUnresolved
	}
	store, err := p.openRecords(false)
	if err != nil {
		return ProwlarrRecord{}, ErrUnresolved
	}
	defer store.close()
	r, err := p.readRecord(store, token)
	if err != nil {
		return ProwlarrRecord{}, ErrUnresolved
	}
	if safe := prowlarrContextError(ctx); safe != nil {
		return ProwlarrRecord{}, safe
	}
	return ProwlarrRecord{
		InstanceHash: r.Instance, CandidateID: r.candidate(), IndexerID: r.IndexerID,
		GUID: r.GUID, Protocol: r.Protocol, Metadata: r.metadata(), HasSize: r.HasSize, Size: r.Size,
		Download: ProwlarrLocator(r.Download), Magnet: ProwlarrLocator(r.Magnet),
	}, nil
}

// Reuse the existing local provider's descriptor-rooted primitives rather than
// importing an Asset/staging lifecycle. Every ancestor is checked/no-follow;
// only missing private config descendants are created, never a symlink alias.
// Config supplies <XDG config>/lernae/acquisition/prowlarr by default.
func (p *Prowlarr) openRecords(create bool) (*localAuthority, error) {
	root := p.config.ReferenceDirectory
	if root == "" || !filepath.IsAbs(root) || root != filepath.Clean(root) || root == "/" {
		return nil, ErrProvider
	}
	ancestor := root
	var missing []string
	for {
		info, err := os.Lstat(ancestor)
		if err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return nil, ErrProvider
			}
			break
		}
		if !create || !errors.Is(err, os.ErrNotExist) {
			return nil, ErrProvider
		}
		parent := filepath.Dir(ancestor)
		if parent == ancestor || parent == "/" {
			return nil, ErrProvider
		}
		missing = append(missing, filepath.Base(ancestor))
		ancestor = parent
	}
	authority, err := localOpenAuthority(ancestor, false)
	if err != nil {
		return nil, ErrProvider
	}
	fail := func() (*localAuthority, error) { authority.close(); return nil, ErrProvider }
	for i := len(missing) - 1; i >= 0; i-- {
		parent := authority.last()
		if !authority.same() {
			return fail()
		}
		if err := parent.root.Mkdir(missing[i], 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return fail()
		}
		child, err := localChild(parent, missing[i], true)
		if err != nil || child.info.Mode().Perm() != 0700 {
			if child != nil {
				child.close()
			}
			return fail()
		}
		if p.syncDirectory(parent.file) != nil {
			child.close()
			return fail()
		}
		authority.names = append(authority.names, missing[i])
		authority.directories = append(authority.directories, child)
	}
	authority.private = true
	if !authority.same() {
		return fail()
	}
	// Provider directory and its two Lernae-owned parents must be private.
	for i := len(authority.directories) - 1; i >= len(authority.directories)-3 && i > 0; i-- {
		info, err := authority.directories[i].file.Stat()
		if err != nil || !localDirectorySafe(info, false, true) || info.Mode().Perm() != 0700 {
			return fail()
		}
	}
	return authority, nil
}

func prowlarrPrivateFile(info os.FileInfo) bool {
	if info == nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || info.Size() <= 0 || info.Size() > prowlarrMaxRecordBytes {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid()) && stat.Nlink == 1
}

func (p *Prowlarr) readRecord(store *localAuthority, token string) (prowlarrRecord, error) {
	if !handlePattern.MatchString(token) || !store.same() {
		return prowlarrRecord{}, ErrUnresolved
	}
	d := store.last()
	file, err := localOpenAt(d.file, token, unix.O_RDONLY, 0)
	if err != nil {
		return prowlarrRecord{}, ErrUnresolved
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !prowlarrPrivateFile(info) {
		return prowlarrRecord{}, ErrUnresolved
	}
	data, err := io.ReadAll(io.LimitReader(file, prowlarrMaxRecordBytes+1))
	if err != nil || len(data) > prowlarrMaxRecordBytes || prowlarrHash("reference-v1", data) != token {
		return prowlarrRecord{}, ErrUnresolved
	}
	var r prowlarrRecord
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&r) != nil || !bytes.Equal(data, r.bytes()) || !p.validRecord(r) {
		return prowlarrRecord{}, ErrUnresolved
	}
	current, err := d.root.Lstat(token)
	after, statErr := file.Stat()
	if err != nil || statErr != nil || !prowlarrPrivateFile(current) || !prowlarrPrivateFile(after) || !os.SameFile(info, current) || localFingerprintOf(info) != localFingerprintOf(after) || !store.same() {
		return prowlarrRecord{}, ErrUnresolved
	}
	return r, nil
}

func (p *Prowlarr) validRecord(r prowlarrRecord) bool {
	if r.Version != 1 || r.Instance != p.instance || !handlePattern.MatchString(r.Instance) || r.IndexerID <= 0 || r.IndexerID > 2147483647 || r.GUID == "" || len(r.GUID) > 8192 || !utf8.ValidString(r.GUID) || strings.ContainsRune(r.GUID, '\x00') || !r.Protocol.valid() || r.Size < 0 || r.Size > 1<<60 || (!r.HasSize && r.Size != 0) || p.containsKey(string(r.bytes())) {
		return false
	}
	if !validOption(Option{ID: r.candidate(), ExecutionRef: r.reference(), Metadata: r.metadata()}) || p.safeTitle(r.Title, r.candidate()) != r.Title {
		return false
	}
	for _, l := range []prowlarrLocator{r.Download, r.Magnet} {
		if l == (prowlarrLocator{}) {
			continue
		}
		// Round-trip typed normalized fields through the same strict route rules.
		q := "link=" + url.QueryEscape(l.Link) + "&file=" + url.QueryEscape(l.File)
		parsed, err := p.normalizeLocator(p.base.Scheme+"://"+p.base.Host+l.Route+"?"+q, r.IndexerID)
		if err != nil || parsed != l {
			return false
		}
	}
	return true
}

func (p *Prowlarr) flushRecord(store *localAuthority, r prowlarrRecord) error {
	file, err := localOpenAt(store.last().file, r.reference(), unix.O_RDONLY, 0)
	if err != nil {
		return ErrProvider
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !prowlarrPrivateFile(info) || p.syncFile(file) != nil || p.syncDirectory(store.last().file) != nil {
		return ErrProvider
	}
	confirmed, err := p.readRecord(store, r.reference())
	current, entryErr := store.last().root.Lstat(r.reference())
	after, statErr := file.Stat()
	if err != nil || confirmed != r || entryErr != nil || statErr != nil || !prowlarrPrivateFile(current) || !prowlarrPrivateFile(after) || !os.SameFile(info, current) || localFingerprintOf(info) != localFingerprintOf(after) || !store.same() {
		return ErrProvider
	}
	return nil
}

func (p *Prowlarr) saveRecord(ctx context.Context, store *localAuthority, r prowlarrRecord) error {
	return p.saveRecordWithReadMissHook(ctx, store, r, nil)
}

// The private hook lets tests schedule a real peer publication in the initial
// read-miss window. Production always passes nil; filesystem operations stay real.
func (p *Prowlarr) saveRecordWithReadMissHook(ctx context.Context, store *localAuthority, r prowlarrRecord, afterReadMiss func()) error {
	if safe := prowlarrContextError(ctx); safe != nil {
		return safe
	}
	if !p.validRecord(r) || !store.same() {
		return ErrProvider
	}
	if existing, err := p.readRecord(store, r.reference()); err == nil {
		if existing != r {
			return ErrProvider
		}
		return p.flushRecord(store, r)
	}
	if afterReadMiss != nil {
		afterReadMiss()
	}
	if _, err := store.last().root.Lstat(r.reference()); err == nil {
		// A peer may have published after the read miss. Reopen and verify once;
		// malformed/unknown entries still fail closed and are never overwritten.
		existing, readErr := p.readRecord(store, r.reference())
		if readErr != nil || existing != r {
			return ErrProvider
		}
		return p.flushRecord(store, r)
	} else if !errors.Is(err, os.ErrNotExist) {
		return ErrProvider
	}
	id, err := newHandle()
	if err != nil {
		return ErrProvider
	}
	name := ".partial-" + id
	d := store.last()
	file, err := localOpenAt(d.file, name, unix.O_CREAT|unix.O_EXCL|unix.O_RDWR, 0600)
	if err != nil {
		return ErrProvider
	}
	info, statErr := file.Stat()
	defer func() { _ = file.Close(); localRemoveOwned(d, name, info) }()
	if statErr != nil || !info.Mode().IsRegular() {
		return ErrProvider
	}
	data := r.bytes()
	n, err := file.Write(data)
	if err != nil || n != len(data) {
		return ErrProvider
	}
	if safe := prowlarrContextError(ctx); safe != nil {
		return safe
	}
	if p.syncFile(file) != nil || !store.same() {
		return ErrProvider
	}
	// No overwrite: concurrent identical publication can only reuse bytes after
	// exact verification and durable flush. Unknown entries never get removed.
	if localPublish(d, name, r.reference()) != nil {
		existing, err := p.readRecord(store, r.reference())
		if err != nil || existing != r {
			return ErrProvider
		}
	}
	if safe := prowlarrContextError(ctx); safe != nil {
		return safe
	}
	return p.flushRecord(store, r)
}
