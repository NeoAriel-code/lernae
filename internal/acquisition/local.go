package acquisition

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const LocalProviderID = "server-local"
const localChunkBytes = 64 * 1024
const localReferencesName = ".local-references"

// Local is simultaneously a discovery provider, exact-selection resolver, and
// productive Server-local executor. It has no Agent, inventory or Asset scope.
// Close must follow ExecutionService.Close and HTTP draining. Accepted handles
// hold a read lock until Wait joins, so descriptors cannot close beneath work.
type Local struct {
	mu         sync.RWMutex
	closed     bool
	source     *localAuthority
	staging    *localAuthority
	references *localDirectory
	// Package-private fault seam; tests still use real opened files and copying.
	syncFile      func(*os.File) error
	syncDirectory func(*os.File) error
}

func (*Local) String() string     { return "Local{roots:<private>}" }
func (l *Local) GoString() string { return l.String() }
func (*Local) ID() string         { return LocalProviderID }

// Five minutes per call supports large local content without weakening exact
// hashes. Discovery shares this total across all candidates; Resolve and Dispatch
// each verify one exact source. It is bounded, not a size/throughput guarantee.
// Accepted copying retains the existing Server-owned cancellation lifecycle.
func (*Local) ContentVerificationTimeouts() ContentVerificationPolicy {
	return ContentVerificationPolicy{
		Discovery: 5 * time.Minute, Resolution: 5 * time.Minute, Dispatch: 5 * time.Minute,
	}
}
func (*Local) Eligible(target Target) bool {
	return localIDPattern.MatchString(string(target.Edition.ID))
}

func OpenLocal(sourceRoot, stagingRoot string) (*Local, error) {
	if err := ValidateLocalRoots(sourceRoot, stagingRoot); err != nil {
		return nil, err
	}
	source, err := localOpenAuthority(sourceRoot, false)
	if err != nil {
		return nil, ErrLocalRoots
	}
	staging, err := localOpenAuthority(stagingRoot, true)
	if err != nil {
		source.close()
		return nil, ErrLocalRoots
	}
	l := &Local{
		source: source, staging: staging,
		syncFile:      func(f *os.File) error { return f.Sync() },
		syncDirectory: func(f *os.File) error { return f.Sync() },
	}
	fail := func() (*Local, error) { _ = l.Close(); return nil, ErrLocalRoots }
	// Descriptor identity prevents alternate configured spellings of one root.
	if !localAuthoritiesSeparate(source, staging) {
		return fail()
	}
	if err := staging.last().root.Mkdir(localReferencesName, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return fail()
	}
	l.references, err = localChild(staging.last(), localReferencesName, true)
	if err != nil || !l.authoritySame() || l.syncDirectory(staging.last().file) != nil {
		return fail()
	}
	return l, nil
}

func (l *Local) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.closed = true
		l.references.close()
		l.source.close()
		l.staging.close()
	}
	return nil
}

func (l *Local) authoritySame() bool {
	return !l.closed && l.source.same() && l.staging.same() && l.references != nil &&
		localChildSame(l.staging.last(), localReferencesName, l.references, true)
}

// A lookup record is provider-owned durable state, not a common plan payload.
// The private reference is SHA-256("reference-v1" || canonical record), and
// candidate identity uses a distinct hash domain. Neither reveals filenames.
// Keeping records in the authorized staging root needs no SQL schema change.
type localRecord struct {
	Version   int
	Edition   string
	Name      string
	Source    localIdentity
	Directory localIdentity
	File      localFingerprint
	Digest    string
}

func localRecordBytes(record localRecord) []byte {
	data, _ := json.Marshal(record)
	return data
}

func localRecordHash(domain string, data []byte) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(domain))
	_, _ = hash.Write(data)
	return hex.EncodeToString(hash.Sum(nil))
}

func (r localRecord) reference() string { return localRecordHash("reference-v1", localRecordBytes(r)) }
func (r localRecord) candidate() string { return localRecordHash("candidate-v1", localRecordBytes(r)) }

func (l *Local) Discover(ctx context.Context, target Target) ([]Option, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if ctx.Err() != nil {
		return nil, ErrCancelled
	}
	if !l.Eligible(target) || !l.authoritySame() {
		return nil, ErrProvider
	}
	edition := string(target.Edition.ID)
	directory, err := localChild(l.source.last(), edition, false)
	if err != nil {
		if _, lookupErr := l.source.last().root.Lstat(edition); errors.Is(lookupErr, os.ErrNotExist) && l.authoritySame() {
			return []Option{}, nil
		}
		return nil, ErrProvider
	}
	defer directory.close()
	entries, err := directory.file.ReadDir(-1)
	if err != nil {
		return nil, ErrProvider
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	options := make([]Option, 0)
	for _, entry := range entries {
		if ctx.Err() != nil {
			return nil, ErrCancelled
		}
		name := entry.Name()
		if !localComponent(name) {
			continue
		}
		info, err := directory.root.Lstat(name)
		if err != nil {
			return nil, ErrProvider
		}
		// Explicitly exclude empty files: Jobs requires a positive byte total. No
		// fake byte, directory recursion, symlink following or special-file open.
		if !info.Mode().IsRegular() || info.Size() <= 0 {
			continue
		}
		file, err := localOpenAt(directory.file, name, unix.O_RDONLY, 0)
		if err != nil {
			return nil, ErrProvider
		}
		opened, err := file.Stat()
		if err != nil || !opened.Mode().IsRegular() || opened.Size() <= 0 || !os.SameFile(info, opened) {
			_ = file.Close()
			return nil, ErrProvider
		}
		digest, err := localDigest(ctx, file, opened.Size())
		after, statErr := file.Stat()
		if err != nil || statErr != nil || localFingerprintOf(opened) != localFingerprintOf(after) ||
			!l.sourceEntrySame(edition, directory, name, opened) {
			_ = file.Close()
			return nil, ErrProvider
		}
		record := localRecord{
			Version: 1, Edition: edition, Name: name,
			Source: localIdentityOf(l.source.last().info), Directory: localIdentityOf(directory.info),
			File: localFingerprintOf(opened), Digest: digest,
		}
		if err := l.saveRecord(ctx, record); err != nil {
			_ = file.Close()
			return nil, err
		}
		final, statErr := file.Stat()
		_ = file.Close()
		if statErr != nil || localFingerprintOf(final) != record.File || !l.sourceEntrySame(edition, directory, name, final) {
			return nil, ErrProvider
		}
		options = append(options, Option{
			ID: record.candidate(), ExecutionRef: record.reference(),
			Metadata: Metadata{Title: "Local file " + record.candidate()[:12], Label: fmt.Sprintf("%d bytes", opened.Size())},
		})
		if len(options) > MaxProviderCandidates {
			return nil, ErrInvalidResponse
		}
	}
	if ctx.Err() != nil {
		return nil, ErrCancelled
	}
	if !l.authoritySame() || !localChildSame(l.source.last(), edition, directory, false) {
		return nil, ErrProvider
	}
	sort.Slice(options, func(i, j int) bool { return options[i].ID < options[j].ID })
	return options, nil
}

func localDigest(ctx context.Context, file *os.File, size int64) (string, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", ErrProvider
	}
	hash := sha256.New()
	buffer := make([]byte, localChunkBytes)
	var count int64
	for {
		if ctx.Err() != nil {
			return "", ErrCancelled
		}
		n, err := file.Read(buffer)
		count += int64(n)
		if count > size {
			return "", ErrProvider
		}
		if n > 0 {
			_, _ = hash.Write(buffer[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil || n == 0 {
			return "", ErrProvider
		}
	}
	if count != size {
		return "", ErrProvider
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (l *Local) sourceEntrySame(edition string, directory *localDirectory, name string, opened os.FileInfo) bool {
	if !l.authoritySame() || !localChildSame(l.source.last(), edition, directory, false) {
		return false
	}
	current, err := directory.root.Lstat(name)
	return err == nil && current.Mode().IsRegular() && os.SameFile(opened, current) && localFingerprintOf(opened) == localFingerprintOf(current)
}

func (l *Local) saveRecord(ctx context.Context, record localRecord) error {
	ref := record.reference()
	if existing, err := l.loadRecord(ref); err == nil {
		if existing == record {
			// A prior publication may have failed its final directory flush.
			// Reusing the lookup must establish durability, not just existence.
			file, err := localOpenAt(l.references.file, ref, unix.O_RDONLY, 0)
			if err != nil {
				return ErrProvider
			}
			defer file.Close()
			info, err := file.Stat()
			if err != nil || !info.Mode().IsRegular() || l.syncFile(file) != nil || l.syncDirectory(l.references.file) != nil || !l.authoritySame() {
				return ErrProvider
			}
			confirmed, err := l.loadRecord(ref)
			if err != nil || confirmed != record {
				return ErrProvider
			}
			return nil
		}
		return ErrProvider
	} else if _, err := l.references.root.Lstat(ref); !errors.Is(err, os.ErrNotExist) {
		return ErrProvider
	}
	id, err := newHandle()
	if err != nil {
		return ErrProvider
	}
	name := ".partial-" + id
	file, err := localOpenAt(l.references.file, name, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL, 0600)
	if err != nil {
		return ErrProvider
	}
	info, err := file.Stat()
	defer func() { _ = file.Close(); localRemoveOwned(l.references, name, info) }()
	if err != nil || !info.Mode().IsRegular() {
		return ErrProvider
	}
	data := localRecordBytes(record)
	n, err := file.Write(data)
	if err != nil || n != len(data) || ctx.Err() != nil || l.syncFile(file) != nil || !l.authoritySame() {
		return ErrProvider
	}
	if err := localPublish(l.references, name, ref); err != nil {
		existing, loadErr := l.loadRecord(ref)
		if loadErr != nil || existing != record {
			return ErrProvider
		}
	}
	if l.syncDirectory(l.references.file) != nil || !l.authoritySame() {
		return ErrProvider
	}
	return nil
}

func (l *Local) loadRecord(ref string) (localRecord, error) {
	if !handlePattern.MatchString(ref) || !l.authoritySame() {
		return localRecord{}, ErrUnresolved
	}
	file, err := localOpenAt(l.references.file, ref, unix.O_RDONLY, 0)
	if err != nil {
		return localRecord{}, ErrUnresolved
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 4096 || info.Mode().Perm()&0077 != 0 {
		return localRecord{}, ErrUnresolved
	}
	data, err := io.ReadAll(io.LimitReader(file, 4097))
	if err != nil || localRecordHash("reference-v1", data) != ref {
		return localRecord{}, ErrUnresolved
	}
	var record localRecord
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&record) != nil || record.Version != 1 || !localIDPattern.MatchString(record.Edition) ||
		!localComponent(record.Name) || record.File.Size <= 0 || !handlePattern.MatchString(record.Digest) || !bytes.Equal(data, localRecordBytes(record)) {
		return localRecord{}, ErrUnresolved
	}
	current, err := l.references.root.Lstat(ref)
	after, statErr := file.Stat()
	if err != nil || statErr != nil || !current.Mode().IsRegular() || !os.SameFile(info, current) || localFingerprintOf(info) != localFingerprintOf(after) || !l.authoritySame() {
		return localRecord{}, ErrUnresolved
	}
	return record, nil
}

// openSelection directly opens the record's exact single component. It never
// scans discovery or falls back to another source when provenance is stale.
func (l *Local) openSelection(ctx context.Context, plan ExecutionPlan) (*localDirectory, *os.File, localRecord, error) {
	reject := func() (*localDirectory, *os.File, localRecord, error) {
		return nil, nil, localRecord{}, ErrUnresolved
	}
	if ctx.Err() != nil || plan.ProviderID != l.ID() || !localIDPattern.MatchString(string(plan.EditionID)) || !localIDPattern.MatchString(plan.JobID) || !handlePattern.MatchString(plan.Handle) {
		return reject()
	}
	record, err := l.loadRecord(plan.ExecutionRef)
	if err != nil || record.Edition != string(plan.EditionID) || record.candidate() != plan.CandidateID || record.Source != localIdentityOf(l.source.last().info) {
		return reject()
	}
	directory, err := localChild(l.source.last(), record.Edition, false)
	if err != nil {
		return reject()
	}
	if record.Directory != localIdentityOf(directory.info) {
		directory.close()
		return reject()
	}
	file, err := localOpenAt(directory.file, record.Name, unix.O_RDONLY, 0)
	if err != nil {
		directory.close()
		return reject()
	}
	fail := func() (*localDirectory, *os.File, localRecord, error) {
		_ = file.Close()
		directory.close()
		return reject()
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || localFingerprintOf(info) != record.File {
		return fail()
	}
	digest, err := localDigest(ctx, file, record.File.Size)
	after, statErr := file.Stat()
	if err != nil || statErr != nil || digest != record.Digest || localFingerprintOf(after) != record.File || !l.sourceEntrySame(record.Edition, directory, record.Name, info) {
		return fail()
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return fail()
	}
	return directory, file, record, nil
}

func (l *Local) Resolve(ctx context.Context, selection Selection) (ExecutionPlan, error) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	plan := PlanForSelection(selection)
	directory, file, _, err := l.openSelection(ctx, plan)
	if err != nil {
		return ExecutionPlan{}, ErrUnresolved
	}
	_ = file.Close()
	directory.close()
	return plan, nil
}

func (l *Local) Dispatch(ctx context.Context, plan ExecutionPlan) (DispatchResult, error) {
	l.mu.RLock()
	reject := func() (DispatchResult, error) {
		l.mu.RUnlock()
		return DispatchResult{Decision: DispatchRejected}, nil
	}
	if !handlePattern.MatchString(plan.ExecutionID) || ctx.Err() != nil {
		return reject()
	}
	directory, file, record, err := l.openSelection(ctx, plan)
	if err != nil {
		return reject()
	}
	fail := func() (DispatchResult, error) {
		_ = file.Close()
		directory.close()
		return reject()
	}
	if ctx.Err() != nil || !l.authoritySame() {
		return fail()
	}
	// mkdir is exclusive. Existing attempts are never reused, removed or retried.
	if err := l.staging.last().root.Mkdir(plan.ExecutionID, 0700); err != nil {
		return fail()
	}
	attempt, err := localChild(l.staging.last(), plan.ExecutionID, true)
	if err != nil {
		return fail() // conservatively retain any uncertain empty directory
	}
	temp, err := localOpenAt(attempt.file, "partial", unix.O_CREAT|unix.O_EXCL|unix.O_RDWR, 0600)
	if err != nil {
		attempt.close()
		return fail()
	}
	tempInfo, err := temp.Stat()
	if err != nil || !tempInfo.Mode().IsRegular() || ctx.Err() != nil || !l.authoritySame() || !localChildSame(l.staging.last(), plan.ExecutionID, attempt, true) {
		localRemoveOwned(attempt, "partial", tempInfo)
		_ = temp.Close()
		attempt.close()
		return fail()
	}
	// Acceptance transfers retained validated source + exclusive writable temp
	// responsibility. No bytes or progress are copied before Wait after running.
	return DispatchResult{Decision: DispatchAccepted, Execution: &localExecution{
		adapter: l, plan: plan, record: record, sourceDirectory: directory, source: file,
		attempt: attempt, temp: temp, tempInfo: tempInfo,
	}}, nil
}

type localExecution struct {
	adapter         *Local
	plan            ExecutionPlan
	record          localRecord
	sourceDirectory *localDirectory
	source          *os.File
	attempt         *localDirectory
	temp            *os.File
	tempInfo        os.FileInfo
	once            sync.Once
	completion      Completion
	err             error
}

func (e *localExecution) Wait(ctx context.Context, report func(Progress) error) (Completion, error) {
	e.once.Do(func() { e.completion, e.err = e.copy(ctx, report) })
	return e.completion, e.err
}

func (e *localExecution) valid() bool {
	l := e.adapter
	info, err := e.source.Stat()
	return err == nil && info.Mode().IsRegular() && localFingerprintOf(info) == e.record.File &&
		l.sourceEntrySame(e.record.Edition, e.sourceDirectory, e.record.Name, info) &&
		localChildSame(l.staging.last(), e.plan.ExecutionID, e.attempt, true)
}

func (e *localExecution) copy(ctx context.Context, report func(Progress) error) (Completion, error) {
	l := e.adapter
	defer func() {
		// Only partial is eligible for cleanup. Published content is retained even
		// if final fsync/authority confirmation fails: no destructive rollback.
		localRemoveOwned(e.attempt, "partial", e.tempInfo)
		_ = e.temp.Close()
		_ = e.source.Close()
		e.sourceDirectory.close()
		e.attempt.close()
		l.mu.RUnlock()
	}()
	failure := func() (Completion, error) {
		if ctx.Err() != nil {
			return CompletionInterrupted, ErrCancelled
		}
		return CompletionFailed, ErrProvider
	}
	if ctx.Err() != nil || report == nil || !e.valid() {
		return failure()
	}
	buffer := make([]byte, localChunkBytes)
	hash := sha256.New()
	var count int64
	for {
		if ctx.Err() != nil || !e.valid() {
			return failure()
		}
		n, err := e.source.Read(buffer)
		if int64(n) > e.record.File.Size-count {
			return failure()
		}
		if n > 0 {
			written, writeErr := e.temp.Write(buffer[:n])
			if writeErr != nil || written != n {
				return failure()
			}
			_, _ = hash.Write(buffer[:n])
			count += int64(n)
			if report(Progress{Current: count, Total: e.record.File.Size}) != nil {
				return failure()
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil || n == 0 {
			return failure()
		}
	}
	if ctx.Err() != nil || count != e.record.File.Size || hex.EncodeToString(hash.Sum(nil)) != e.record.Digest || !e.valid() {
		return failure()
	}
	// Verify both actual source and actual staged descriptors after copying, not
	// merely expected size/mtime or a digest of the buffer before filesystem write.
	sourceDigest, err := localDigest(ctx, e.source, e.record.File.Size)
	if err != nil || sourceDigest != e.record.Digest || !e.valid() {
		return failure()
	}
	tempDigest, err := localDigest(ctx, e.temp, e.record.File.Size)
	info, statErr := e.temp.Stat()
	entry, pathErr := e.attempt.root.Lstat("partial")
	if err != nil || statErr != nil || pathErr != nil || tempDigest != e.record.Digest || !info.Mode().IsRegular() ||
		info.Size() != count || !os.SameFile(info, e.tempInfo) || !entry.Mode().IsRegular() || !os.SameFile(info, entry) {
		return failure()
	}
	if l.syncFile(e.temp) != nil || l.syncDirectory(e.attempt.file) != nil || l.syncDirectory(l.staging.last().file) != nil || ctx.Err() != nil || !e.valid() {
		return failure()
	}
	flushed, flushErr := e.temp.Stat()
	current, currentErr := e.attempt.root.Lstat("partial")
	if flushErr != nil || currentErr != nil || localFingerprintOf(flushed) != localFingerprintOf(info) || !current.Mode().IsRegular() || !os.SameFile(flushed, current) {
		return failure()
	}
	if err := e.temp.Close(); err != nil {
		return failure()
	}
	if err := localPublish(e.attempt, "partial", "content"); err != nil {
		return failure()
	}
	if l.syncDirectory(e.attempt.file) != nil || l.syncDirectory(l.staging.last().file) != nil || !e.valid() {
		return failure()
	}
	published, err := e.attempt.root.Lstat("content")
	if err != nil || !published.Mode().IsRegular() || !os.SameFile(info, published) || published.Size() != count {
		return failure()
	}
	return CompletionSucceeded, nil
}
