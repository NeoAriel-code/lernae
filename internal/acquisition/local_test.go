package acquisition

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"lernae/internal/database"
	"lernae/internal/domain"
	"lernae/internal/jobs"
)

func TestLocalStagingReservesExactAssetsComponent(t *testing.T) {
	base := t.TempDir()
	source := filepath.Join(base, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{"assets", "assets/staging", "cache/assets", "cache/assets/staging"} {
		staging := filepath.Join(base, relative)
		if err := os.MkdirAll(staging, 0700); err != nil {
			t.Fatal(err)
		}
		if err := ValidateLocalRoots(source, staging); err == nil {
			t.Errorf("reserved staging syntax accepted: %s", relative)
		}
		if err := CheckLocalRoots(source, staging); err == nil {
			t.Errorf("reserved actual staging accepted: %s", relative)
		}
		if adapter, err := OpenLocal(source, staging); err == nil {
			_ = adapter.Close()
			t.Errorf("reserved staging opened: %s", relative)
		}
		if _, err := os.Stat(filepath.Join(staging, localReferencesName)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("refusal created provider state: %s", relative)
		}
	}
	for _, relative := range []string{"Assets", "my-assets", "assets-backup"} {
		staging := filepath.Join(base, relative)
		if err := os.Mkdir(staging, 0700); err != nil {
			t.Fatal(err)
		}
		adapter, err := OpenLocal(source, staging)
		if err != nil {
			t.Fatal("nonreserved component refused", err)
		}
		_ = adapter.Close()
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(filepath.Join(base, "assets"), alias); err != nil {
		t.Fatal(err)
	}
	for _, staging := range []string{alias, alias + "/staging", base + "/assets/../Assets", base + "//Assets"} {
		if adapter, err := OpenLocal(source, staging); err == nil {
			_ = adapter.Close()
			t.Error("alias or traversal bypass accepted")
		}
	}
	// Source is read-only: the reserved component does not apply there.
	adapter, err := OpenLocal(filepath.Join(base, "assets"), filepath.Join(base, "Assets"))
	if err != nil {
		t.Fatal("read-only assets source refused", err)
	}
	_ = adapter.Close()
}

func TestLocalInvalidUTF8FilenameNeverCreatesSelectableRecord(t *testing.T) {
	adapter, source, staging := localFixture(t)
	writeLocal(t, source, "bad-"+string([]byte{0xff})+".bin", []byte("real raw-byte filename"))
	options, err := adapter.Discover(context.Background(), localTarget())
	if err != nil || len(options) != 0 {
		t.Errorf("invalid UTF-8 produced selectable candidate: count=%d err=%v", len(options), err)
	}
	entries, err := os.ReadDir(filepath.Join(staging, localReferencesName))
	if err != nil || len(entries) != 0 {
		t.Error("invalid UTF-8 persisted a corrupt private record")
	}
	if localComponent("bad-" + string([]byte{0xff})) {
		t.Error("private record filename validator accepts invalid UTF-8")
	}
	// A valid Unicode replacement-character name is distinct from the raw bytes.
	writeLocal(t, source, "bad-\ufffd.bin", []byte("different real file"))
	choice := localChoice(t, adapter)
	record, err := adapter.loadRecord(choice.Candidate.Option.ExecutionRef)
	if err != nil || record.Name != "bad-\ufffd.bin" {
		t.Fatal("valid Unicode name was confused with raw-byte name", err)
	}
	// Even content-address-valid private JSON with invalid filename bytes refuses.
	data := bytes.Replace(localRecordBytes(record), []byte("bad-\ufffd.bin"), []byte("bad-\xff.bin"), 1)
	ref := localRecordHash("reference-v1", data)
	if err := os.WriteFile(filepath.Join(staging, localReferencesName, ref), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.loadRecord(ref); err != ErrUnresolved {
		t.Fatal("invalid raw-byte private record accepted", err)
	}
}

// Capture budgets at real filesystem calls, without sleeping or replacing hashes.
type budgetLocal struct {
	*Local
	t *testing.T
}

func (l budgetLocal) budget(ctx context.Context) {
	l.t.Helper()
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) <= 4*time.Minute || time.Until(deadline) > 5*time.Minute {
		l.t.Error("local content verification did not receive a bounded >2s budget")
	}
}
func (l budgetLocal) Discover(ctx context.Context, target Target) ([]Option, error) {
	l.budget(ctx)
	return l.Local.Discover(ctx, target)
}
func (l budgetLocal) Resolve(ctx context.Context, selected Selection) (ExecutionPlan, error) {
	l.budget(ctx)
	return l.Local.Resolve(ctx, selected)
}
func (l budgetLocal) Dispatch(ctx context.Context, plan ExecutionPlan) (DispatchResult, error) {
	l.budget(ctx)
	return l.Local.Dispatch(ctx, plan)
}

func TestLocalCoreContentVerificationBudgetsAndRealCopy(t *testing.T) {
	adapter, source, staging := localFixture(t)
	data := bytes.Repeat([]byte("budgeted real filesystem bytes"), 5000)
	writeLocal(t, source, "file", data)
	local := budgetLocal{Local: adapter, t: t}
	service, jobService, db, _, job := fixture(t, local)
	found, err := service.Discover(context.Background(), job.ID)
	if err != nil || len(found.Candidates) != 1 {
		t.Fatal("discovery failed", err)
	}
	if _, err := service.Select(context.Background(), job.ID, found.Candidates[0].Handle); err != nil {
		t.Fatal(err)
	}
	registry, err := NewExecutionRegistry(local)
	if err != nil {
		t.Fatal(err)
	}
	core := NewExecutionService(context.Background(), jobService, NewSQLiteRepository(db), registry, local)
	defer core.Close()
	reservation, err := core.Execute(context.Background(), job.ID)
	if err != nil || reservation.Outcome != DispatchAccepted {
		t.Fatal("execution failed", err)
	}
	core.Wait()
	copied, err := os.ReadFile(filepath.Join(staging, reservation.ExecutionID, "content"))
	if err != nil || !bytes.Equal(data, copied) {
		t.Fatal("budgeted execution did not copy exact real bytes", err)
	}
}

func localFixture(t *testing.T) (*Local, string, string) {
	t.Helper()
	base := t.TempDir()
	source, staging := filepath.Join(base, "source"), filepath.Join(base, "staging")
	for _, path := range []string{source, staging, filepath.Join(source, "edition-1")} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	adapter, err := OpenLocal(source, staging)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adapter.Close() })
	return adapter, source, staging
}

func localTarget() Target { return Target{Edition: domain.Edition{ID: "edition-1"}} }

func writeLocal(t *testing.T, source, name string, data []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(source, "edition-1", name), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func localChoice(t *testing.T, adapter *Local) Selection {
	t.Helper()
	options, err := adapter.Discover(context.Background(), localTarget())
	if err != nil || len(options) != 1 {
		t.Fatalf("options=%v err=%v", options, err)
	}
	return Selection{Candidate: Candidate{
		JobID: "job-1", EditionID: "edition-1", ProviderID: adapter.ID(), Handle: strings.Repeat("a", 64), Option: options[0],
	}}
}

func localDispatch(t *testing.T, adapter *Local, choice Selection, identity string) Execution {
	t.Helper()
	plan, err := adapter.Resolve(context.Background(), choice)
	if err != nil {
		t.Fatal(err)
	}
	plan.ExecutionID = identity
	result, err := adapter.Dispatch(context.Background(), plan)
	if err != nil || result.Decision != DispatchAccepted || result.Execution == nil {
		t.Fatalf("dispatch=%v err=%v", result.Decision, err)
	}
	return result.Execution
}

func TestLocalDeterministicDirectCandidatesAndExactCopy(t *testing.T) {
	adapter, source, staging := localFixture(t)
	data := bytes.Repeat([]byte("actual local bytes"), 10000)
	writeLocal(t, source, "PRIVATE_FILE_NAME", data)
	writeLocal(t, source, "other", []byte("not selected"))
	writeLocal(t, source, "empty", nil)
	if err := os.Mkdir(filepath.Join(source, "edition-1", "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("PRIVATE_FILE_NAME", filepath.Join(source, "edition-1", "link")); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(filepath.Join(source, "edition-1", "fifo"), 0600); err != nil {
		t.Fatal(err)
	}
	first, err := adapter.Discover(context.Background(), localTarget())
	if err != nil || len(first) != 2 {
		t.Fatalf("discovery count=%d err=%v", len(first), err)
	}
	second, err := adapter.Discover(context.Background(), localTarget())
	if err != nil || len(second) != 2 || first[0] != second[0] || first[1] != second[1] || first[0].ID >= first[1].ID {
		t.Fatal("non-deterministic discovery")
	}
	for _, option := range first {
		if !validOption(option) || strings.Contains(option.Metadata.Title, "PRIVATE") || strings.Contains(option.Metadata.Label, source) {
			t.Fatal("invalid or private metadata")
		}
	}
	// Read provider-owned records in the test to prove each exact selection,
	// without deriving a filesystem name from its public label.
	for i, option := range first {
		choice := Selection{Candidate: Candidate{
			JobID: "job-1", EditionID: "edition-1", ProviderID: adapter.ID(), Handle: strings.Repeat("a", 64), Option: option,
		}}
		record, err := adapter.loadRecord(option.ExecutionRef)
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join(source, "edition-1", record.Name))
		if err != nil {
			t.Fatal(err)
		}
		id := strings.Repeat(string(rune('b'+i)), 64)
		work := localDispatch(t, adapter, choice, id)
		previous := int64(-1)
		var total int64
		completion, err := work.Wait(context.Background(), func(p Progress) error {
			if p.Total <= 0 || p.Current <= previous || p.Current > p.Total {
				t.Fatalf("unreal progress=%v previous=%d", p, previous)
			}
			previous, total = p.Current, p.Total
			return nil
		})
		if err != nil || completion != CompletionSucceeded || previous != total {
			t.Fatalf("completion=%v err=%v progress=%d/%d", completion, err, previous, total)
		}
		copied, err := os.ReadFile(filepath.Join(staging, id, "content"))
		if err != nil || int64(len(copied)) != total || !bytes.Equal(copied, want) {
			t.Fatal("staged bytes differ from exact selection")
		}
		if _, err := os.Stat(filepath.Join(staging, id, "partial")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("partial left after success")
		}
		// Wait is a joined, one-shot handle, not permission to copy again.
		if _, err := work.Wait(context.Background(), func(Progress) error {
			t.Fatal("second Wait copied bytes again")
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLocalSelectionSurvivesAdapterRestartWithoutDiscovery(t *testing.T) {
	adapter, source, staging := localFixture(t)
	writeLocal(t, source, "exact", []byte("selected bytes"))
	choice := localChoice(t, adapter)
	if err := adapter.Close(); err != nil {
		t.Fatal(err)
	}
	fresh, err := OpenLocal(source, staging)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	work := localDispatch(t, fresh, choice, strings.Repeat("d", 64))
	if completion, err := work.Wait(context.Background(), func(Progress) error { return nil }); err != nil || completion != CompletionSucceeded {
		t.Fatalf("restart completion=%v err=%v", completion, err)
	}
}

func TestLocalRejectsMissingReplacedAndSameSizeSameMtimeContentChange(t *testing.T) {
	for _, mode := range []string{"missing", "replaced", "mutated", "symlink", "directory swap", "root swap"} {
		t.Run(mode, func(t *testing.T) {
			adapter, source, _ := localFixture(t)
			writeLocal(t, source, "exact", []byte("original"))
			choice := localChoice(t, adapter)
			path := filepath.Join(source, "edition-1", "exact")
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "missing":
				err = os.Rename(path, path+"-gone")
			case "replaced":
				err = os.Rename(path, path+"-old")
				if err == nil {
					writeLocal(t, source, "exact", []byte("original"))
				}
			case "mutated":
				writeLocal(t, source, "exact", []byte("mutated!"))
				err = os.Chtimes(path, info.ModTime(), info.ModTime())
			case "symlink":
				err = os.Rename(path, path+"-old")
				if err == nil {
					err = os.Symlink(path+"-old", path)
				}
			case "directory swap":
				err = os.Rename(filepath.Join(source, "edition-1"), filepath.Join(source, "old-edition"))
				if err == nil {
					err = os.Mkdir(filepath.Join(source, "edition-1"), 0700)
				}
				if err == nil {
					writeLocal(t, source, "exact", []byte("original"))
				}
			case "root swap":
				err = os.Rename(source, source+"-old")
				if err == nil {
					err = os.Mkdir(source, 0700)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := adapter.Resolve(context.Background(), choice); err == nil {
				t.Fatal("changed selection resolved")
			}
		})
	}
}

func TestLocalCancellationCollisionAndLateMutationCleanup(t *testing.T) {
	for _, mode := range []string{"cancel", "progress failure", "late mutation", "collision", "staging swap"} {
		t.Run(mode, func(t *testing.T) {
			adapter, source, staging := localFixture(t)
			data := bytes.Repeat([]byte("real chunks"), 10000)
			writeLocal(t, source, "exact", data)
			choice := localChoice(t, adapter)
			id := strings.Repeat("e", 64)
			work := localDispatch(t, adapter, choice, id)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			called := false
			completion, err := work.Wait(ctx, func(p Progress) error {
				if called {
					return nil
				}
				called = true
				if p.Current <= 0 || p.Current >= p.Total {
					t.Fatal("need real intermediate bytes")
				}
				switch mode {
				case "cancel":
					cancel()
				case "progress failure":
					return errors.New("private callback failure")
				case "late mutation":
					writeLocal(t, source, "exact", bytes.Repeat([]byte("x"), len(data)))
				case "collision":
					if err := os.WriteFile(filepath.Join(staging, id, "content"), []byte("keep collision"), 0600); err != nil {
						t.Fatal(err)
					}
				case "staging swap":
					if err := os.Rename(staging, staging+"-old"); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(staging, 0700); err != nil {
						t.Fatal(err)
					}
				}
				return nil
			})
			if !called || err == nil || completion == CompletionSucceeded {
				t.Fatalf("unsafe completion=%v err=%v", completion, err)
			}
			inspect := staging
			if mode == "staging swap" {
				inspect += "-old"
			}
			if _, err := os.Stat(filepath.Join(inspect, id, "partial")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("owned partial was not cleaned")
			}
			if mode == "collision" {
				got, err := os.ReadFile(filepath.Join(staging, id, "content"))
				if err != nil || string(got) != "keep collision" {
					t.Fatal("collision overwritten or removed")
				}
			} else if _, err := os.Stat(filepath.Join(inspect, id, "content")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed transfer published")
			}
		})
	}
}

func TestLocalExistingReferenceRequiresDurableFlushBeforeReuse(t *testing.T) {
	adapter, source, _ := localFixture(t)
	writeLocal(t, source, "exact", []byte("real lookup bytes"))
	choice := localChoice(t, adapter)
	adapter.syncDirectory = func(*os.File) error { return errors.New("private flush failure") }
	if _, err := adapter.Discover(context.Background(), localTarget()); err == nil {
		t.Fatal("existing lookup reused without a durable directory flush")
	}
	adapter.syncDirectory = func(f *os.File) error { return f.Sync() }
	options, err := adapter.Discover(context.Background(), localTarget())
	if err != nil || len(options) != 1 || options[0] != choice.Candidate.Option {
		t.Fatal("healthy lookup flush did not preserve exact identity")
	}
}

func TestLocalDurabilityFaultsNeverClaimSuccessOrDeletePublishedContent(t *testing.T) {
	for _, mode := range []string{"file sync", "before publication directory sync", "after publication directory sync"} {
		t.Run(mode, func(t *testing.T) {
			adapter, source, staging := localFixture(t)
			writeLocal(t, source, "exact", []byte("durable real bytes"))
			choice := localChoice(t, adapter)
			identity := strings.Repeat("f", 64)
			work := localDispatch(t, adapter, choice, identity)
			if mode == "file sync" {
				adapter.syncFile = func(*os.File) error { return errors.New("private sync failure") }
			}
			syncs := 0
			adapter.syncDirectory = func(file *os.File) error {
				syncs++
				if (mode == "before publication directory sync" && syncs == 1) || (mode == "after publication directory sync" && syncs == 3) {
					return errors.New("private directory failure")
				}
				return file.Sync()
			}
			completion, err := work.Wait(context.Background(), func(Progress) error { return nil })
			if err == nil || completion == CompletionSucceeded || strings.Contains(err.Error(), "private") {
				t.Fatalf("fault completion=%v err=%v", completion, err)
			}
			if _, err := os.Stat(filepath.Join(staging, identity, "partial")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("partial not cleaned")
			}
			_, err = os.Stat(filepath.Join(staging, identity, "content"))
			if mode == "after publication directory sync" {
				if err != nil {
					t.Fatal("uncertain published bytes destructively removed")
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed flush published content")
			}
		})
	}
}

func TestLocalDispatchRejectsChangedSourceAndPreexistingAttempt(t *testing.T) {
	for _, mode := range []string{"changed", "fifo", "directory collision"} {
		t.Run(mode, func(t *testing.T) {
			adapter, source, staging := localFixture(t)
			writeLocal(t, source, "exact", []byte("original bytes"))
			choice := localChoice(t, adapter)
			plan, err := adapter.Resolve(context.Background(), choice)
			if err != nil {
				t.Fatal(err)
			}
			plan.ExecutionID = strings.Repeat("b", 64)
			switch mode {
			case "changed":
				writeLocal(t, source, "exact", []byte("modified bytes"))
			case "fifo":
				path := filepath.Join(source, "edition-1", "exact")
				if err := os.Rename(path, path+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := unix.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "directory collision":
				if err := os.Mkdir(filepath.Join(staging, plan.ExecutionID), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(staging, plan.ExecutionID, "partial"), []byte("keep untouched"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			result, err := adapter.Dispatch(context.Background(), plan)
			if err != nil || result.Decision != DispatchRejected || result.Execution != nil {
				t.Fatalf("dispatch=%v err=%v", result.Decision, err)
			}
			if mode == "directory collision" {
				data, err := os.ReadFile(filepath.Join(staging, plan.ExecutionID, "partial"))
				if err != nil || string(data) != "keep untouched" {
					t.Fatal("unowned collision changed")
				}
			}
		})
	}
}

func TestLocalManipulatedPrivateRecordCannotTraverseOrSubstitute(t *testing.T) {
	adapter, source, _ := localFixture(t)
	writeLocal(t, source, "exact", []byte("selected"))
	choice := localChoice(t, adapter)
	original, err := adapter.loadRecord(choice.Candidate.Option.ExecutionRef)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../outside", "/outside", "nested/file", "nested\\file", "."} {
		record := original
		record.Name = name
		if err := adapter.saveRecord(context.Background(), record); err != nil {
			t.Fatal(err)
		}
		altered := choice
		altered.Candidate.Option.ExecutionRef = record.reference()
		altered.Candidate.Option.ID = record.candidate()
		if _, err := adapter.Resolve(context.Background(), altered); err == nil {
			t.Fatal("private record traversal accepted")
		}
	}
	record := original
	record.Edition = "another-edition"
	if err := adapter.saveRecord(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	altered := choice
	altered.Candidate.Option.ExecutionRef = record.reference()
	if _, err := adapter.Resolve(context.Background(), altered); err == nil {
		t.Fatal("private edition substitution accepted")
	}
}

type localCountingExecutor struct {
	local   *Local
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (e *localCountingExecutor) Dispatch(ctx context.Context, plan ExecutionPlan) (DispatchResult, error) {
	e.calls.Add(1)
	close(e.entered)
	select {
	case <-ctx.Done():
		return DispatchResult{}, ctx.Err()
	case <-e.release:
	}
	return e.local.Dispatch(ctx, plan)
}

func TestLocalRealCopyTwoPoolsConcurrentRetryAndRestartDoNotCopyTwice(t *testing.T) {
	adapter, source, staging := localFixture(t)
	data := bytes.Repeat([]byte("one exact local transfer"), 3000)
	writeLocal(t, source, "exact", data)
	selections, jobService, db, path, job := fixture(t, adapter)
	discovered, err := selections.Discover(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := selections.Select(context.Background(), job.ID, discovered.Candidates[0].Handle); err != nil {
		t.Fatal(err)
	}
	secondDB, err := database.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer secondDB.Close()
	registry, _ := NewExecutionRegistry(adapter)
	executor := &localCountingExecutor{local: adapter, entered: make(chan struct{}), release: make(chan struct{})}
	first := NewExecutionService(context.Background(), jobService, NewSQLiteRepository(db), registry, executor)
	second := NewExecutionService(context.Background(), jobs.NewService(jobs.NewSQLiteRepository(secondDB)), NewSQLiteRepository(secondDB), registry, executor)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(executor.release) }) }
	defer func() { release(); first.Close(); second.Close() }()
	type response struct {
		receipt Reservation
		err     error
	}
	responses := make(chan response, 12)
	for i := range 12 {
		go func(i int) {
			core := first
			if i%2 == 1 {
				core = second
			}
			receipt, err := core.Execute(context.Background(), job.ID)
			responses <- response{receipt, err}
		}(i)
	}
	<-executor.entered
	// Eleven losers return the stored reserved receipt while actual dispatch is
	// blocked; no sleep-based assumption about scheduling or a mocked copy.
	var identity string
	for range 11 {
		response := <-responses
		if response.err != nil || response.receipt.Outcome != DispatchReserved {
			t.Fatalf("loser=%v err=%v", response.receipt, response.err)
		}
		if identity != "" && identity != response.receipt.ExecutionID {
			t.Fatal("multiple attempt identities")
		}
		identity = response.receipt.ExecutionID
	}
	release()
	winner := <-responses
	if winner.err != nil || winner.receipt.Outcome != DispatchAccepted || winner.receipt.ExecutionID != identity {
		t.Fatalf("winner=%v err=%v", winner.receipt, winner.err)
	}
	first.Wait()
	second.Wait()
	if executor.calls.Load() != 1 {
		t.Fatal("duplicate dispatch")
	}
	loaded, err := jobService.GetAcquisition(context.Background(), job.ID)
	if err != nil || loaded.Status != jobs.StatusSucceeded || loaded.ProgressCurrent != int64(len(data)) {
		t.Fatalf("terminal=%v err=%v", loaded.Status, err)
	}
	copied, err := os.ReadFile(filepath.Join(staging, identity, "content"))
	if err != nil || !bytes.Equal(copied, data) {
		t.Fatal("real copied bytes mismatch")
	}
	if _, err := jobService.ReconcileStartup(context.Background()); err != nil {
		t.Fatal(err)
	}
	restarted := NewExecutionService(context.Background(), jobService, NewSQLiteRepository(db), nil, nil)
	defer restarted.Close()
	retry, err := restarted.Execute(context.Background(), job.ID)
	if err != nil || retry != winner.receipt || executor.calls.Load() != 1 {
		t.Fatal("restart replay lost permanent reservation")
	}
}

type localObservedExecutor struct {
	local   *Local
	observe func(context.Context, Progress) error
}

func (e localObservedExecutor) Dispatch(ctx context.Context, plan ExecutionPlan) (DispatchResult, error) {
	result, err := e.local.Dispatch(ctx, plan)
	if result.Execution != nil {
		result.Execution = localObservedExecution{work: result.Execution, observe: e.observe}
	}
	return result, err
}

type localObservedExecution struct {
	work    Execution
	observe func(context.Context, Progress) error
}

func (e localObservedExecution) Wait(ctx context.Context, report func(Progress) error) (Completion, error) {
	return e.work.Wait(ctx, func(progress Progress) error {
		if err := report(progress); err != nil {
			return err
		}
		return e.observe(ctx, progress)
	})
}

func TestLocalRealRunningJobCancellationJoinsAndCleansPartial(t *testing.T) {
	adapter, source, staging := localFixture(t)
	data := bytes.Repeat([]byte("real cancellation bytes"), 10000)
	writeLocal(t, source, "exact", data)
	selections, jobService, db, _, job := fixture(t, adapter)
	discovered, err := selections.Discover(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := selections.Select(context.Background(), job.ID, discovered.Candidates[0].Handle); err != nil {
		t.Fatal(err)
	}
	registry, _ := NewExecutionRegistry(adapter)
	entered := make(chan struct{})
	executor := localObservedExecutor{local: adapter, observe: func(ctx context.Context, progress Progress) error {
		close(entered)
		<-ctx.Done()
		return ctx.Err()
	}}
	core := NewExecutionService(context.Background(), jobService, NewSQLiteRepository(db), registry, executor)
	defer core.Close()
	receipt, err := core.Execute(context.Background(), job.ID)
	if err != nil || receipt.Outcome != DispatchAccepted {
		t.Fatalf("acceptance=%v err=%v", receipt, err)
	}
	<-entered
	running, err := jobService.GetAcquisition(context.Background(), job.ID)
	if err != nil || running.Status != jobs.StatusRunning || running.ProgressCurrent <= 0 || running.ProgressCurrent >= int64(len(data)) || running.ProgressTotal != int64(len(data)) {
		t.Fatalf("actual running byte progress=%v err=%v", running, err)
	}
	core.Close()
	terminal, err := jobService.GetAcquisition(context.Background(), job.ID)
	if err != nil || terminal.Status != jobs.StatusInterrupted {
		t.Fatalf("cancelled terminal=%v err=%v", terminal.Status, err)
	}
	for _, name := range []string{"partial", "content"} {
		if _, err := os.Stat(filepath.Join(staging, receipt.ExecutionID, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("cancelled real Job retained partial or published content")
		}
	}
}

func TestLocalMissingEditionEmptyAndSymlinkEditionFailsClosed(t *testing.T) {
	adapter, source, _ := localFixture(t)
	options, err := adapter.Discover(context.Background(), localTarget())
	if err != nil || len(options) != 0 {
		t.Fatal("empty edition directory not empty")
	}
	target := localTarget()
	target.Edition.ID = "missing-edition"
	options, err = adapter.Discover(context.Background(), target)
	if err != nil || len(options) != 0 {
		t.Fatal("absent edition directory not empty")
	}
	if err := os.Symlink(filepath.Join(source, "edition-1"), filepath.Join(source, "symlink-edition")); err != nil {
		t.Fatal(err)
	}
	target.Edition.ID = "symlink-edition"
	if _, err := adapter.Discover(context.Background(), target); err == nil {
		t.Fatal("symlink edition discovered")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := adapter.Discover(ctx, localTarget()); err == nil {
		t.Fatal("cancelled discovery ignored context")
	}
}

func TestLocalTraversalAndInvalidRoots(t *testing.T) {
	adapter, source, staging := localFixture(t)
	writeLocal(t, source, "exact", []byte("bytes"))
	choice := localChoice(t, adapter)
	for _, id := range []string{"../edition-1", "/edition-1", "edition-1/child", "edition-1\\child", ".", ".."} {
		target := localTarget()
		target.Edition.ID = domain.EditionID(id)
		if adapter.Eligible(target) {
			t.Fatal("unsafe target eligible")
		}
		if _, err := adapter.Discover(context.Background(), target); err == nil {
			t.Fatal("unsafe target discovered")
		}
		bad := choice
		bad.Candidate.EditionID = domain.EditionID(id)
		if _, err := adapter.Resolve(context.Background(), bad); err == nil {
			t.Fatal("unsafe selection resolved")
		}
	}
	for _, ref := range []string{"../outside", "/outside", strings.Repeat("f", 64), choice.Candidate.Option.ExecutionRef + "x"} {
		bad := choice
		bad.Candidate.Option.ExecutionRef = ref
		if _, err := adapter.Resolve(context.Background(), bad); err == nil {
			t.Fatal("manipulated reference resolved")
		}
	}
	for _, roots := range [][2]string{
		{"", staging}, {"relative", staging}, {"/", staging}, {source, source},
		{source, filepath.Join(source, "edition-1")}, {source, filepath.Join(staging, "missing")},
		{source, filepath.Dir(source)},
	} {
		if opened, err := OpenLocal(roots[0], roots[1]); err == nil {
			opened.Close()
			t.Fatalf("invalid roots accepted: %v", roots)
		}
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	if opened, err := OpenLocal(link, staging); err == nil {
		opened.Close()
		t.Fatal("symlink root accepted")
	}
}
