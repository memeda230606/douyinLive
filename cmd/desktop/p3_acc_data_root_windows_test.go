//go:build p3accacceptance && windows

package main

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func p3ACCDataRootTestGeneration(seed uint64) p3ACCDataRootGeneration {
	return p3ACCDataRootGeneration{
		volumeSerialNumber: uint32(seed),
		fileIndex:          seed + 100,
		creationTime100NS:  seed + 200,
	}
}

func TestP3ACCDataRootPhysicalTrackerPreservesLastCompleteBaseline(t *testing.T) {
	resetP3ACCDataRootPhysicalTracker()
	defer resetP3ACCDataRootPhysicalTracker()
	generation := p3ACCDataRootTestGeneration(1)
	if value, complete := p3ACCUpdateDataRootPhysicalTracker(100, generation, true); value != 0 || complete {
		t.Fatalf("first footprint sample = (%d, %v)", value, complete)
	}
	if value, complete := p3ACCUpdateDataRootPhysicalTracker(140, generation, true); value != 40 || !complete {
		t.Fatalf("positive footprint growth = (%d, %v)", value, complete)
	}
	if value, complete := p3ACCUpdateDataRootPhysicalTracker(80, generation, true); value != 40 || !complete {
		t.Fatalf("footprint shrink invented bytes = (%d, %v)", value, complete)
	}
	if value, complete := p3ACCUpdateDataRootPhysicalTracker(100, generation, true); value != 60 || !complete {
		t.Fatalf("post-shrink regrowth = (%d, %v)", value, complete)
	}
	if value, complete := p3ACCUpdateDataRootPhysicalTracker(
		0, p3ACCDataRootTestGeneration(2), false,
	); value != 60 || complete {
		t.Fatalf("incomplete footprint sample = (%d, %v)", value, complete)
	}
	if value, complete := p3ACCUpdateDataRootPhysicalTracker(160, generation, true); value != 120 || !complete {
		t.Fatalf("post-transient growth lost the last complete baseline: (%d, %v)", value, complete)
	}
}

func TestP3ACCDataRootPhysicalTrackerPinsGenerationAndCreationTime(t *testing.T) {
	resetP3ACCDataRootPhysicalTracker()
	defer resetP3ACCDataRootPhysicalTracker()
	generation := p3ACCDataRootTestGeneration(1)
	changedCreation := generation
	changedCreation.creationTime100NS++
	if value, complete := p3ACCUpdateDataRootPhysicalTracker(100, generation, true); value != 0 || complete {
		t.Fatalf("root-generation baseline = (%d, %v)", value, complete)
	}
	if value, complete := p3ACCUpdateDataRootPhysicalTracker(
		0, changedCreation, false,
	); value != 0 || complete {
		t.Fatalf("transient generation sample = (%d, %v)", value, complete)
	}
	if value, complete := p3ACCUpdateDataRootPhysicalTracker(120, generation, true); value != 20 || !complete {
		t.Fatalf("transient sample changed pinned generation: (%d, %v)", value, complete)
	}
	if value, complete := p3ACCUpdateDataRootPhysicalTracker(140, changedCreation, true); value != 0 || complete {
		t.Fatalf("creation-time generation mismatch was accepted: (%d, %v)", value, complete)
	}
	if value, complete := p3ACCUpdateDataRootPhysicalTracker(160, generation, true); value != 0 || complete {
		t.Fatalf("generation poison was implicitly reset: (%d, %v)", value, complete)
	}

	resetP3ACCDataRootPhysicalTracker()
	if value, complete := p3ACCUpdateDataRootPhysicalTracker(
		100, p3ACCDataRootGeneration{}, true,
	); value != 0 || complete {
		t.Fatalf("zero generation was accepted: (%d, %v)", value, complete)
	}
	if value, complete := p3ACCUpdateDataRootPhysicalTracker(120, generation, true); value != 0 || complete {
		t.Fatalf("zero-generation poison was bypassed: (%d, %v)", value, complete)
	}
	resetP3ACCDataRootPhysicalTracker()
	if value, complete := p3ACCUpdateDataRootPhysicalTracker(120, generation, true); value != 0 || complete {
		t.Fatalf("post-reset generation baseline = (%d, %v)", value, complete)
	}
	if value, complete := p3ACCUpdateDataRootPhysicalTracker(121, generation, true); value != 1 || !complete {
		t.Fatalf("post-reset generation recovery = (%d, %v)", value, complete)
	}
}

func TestP3ACCDataRootPhysicalTrackerOverflowStaysPoisonedUntilExplicitReset(t *testing.T) {
	resetP3ACCDataRootPhysicalTracker()
	defer resetP3ACCDataRootPhysicalTracker()
	generation := p3ACCDataRootTestGeneration(1)
	if value, complete := p3ACCUpdateDataRootPhysicalTracker(0, generation, false); value != 0 || complete {
		t.Fatalf("initial transient sample = (%d, %v)", value, complete)
	}
	if value, complete := p3ACCUpdateDataRootPhysicalTracker(100, generation, true); value != 0 || complete {
		t.Fatalf("first complete sample after transient = (%d, %v)", value, complete)
	}

	p3ACCDataRootPhysicalTracker.Lock()
	p3ACCDataRootPhysicalTracker.initialized = true
	p3ACCDataRootPhysicalTracker.previous = 0
	p3ACCDataRootPhysicalTracker.cumulative = math.MaxInt64
	p3ACCDataRootPhysicalTracker.generation = generation
	p3ACCDataRootPhysicalTracker.Unlock()
	if value, complete := p3ACCUpdateDataRootPhysicalTracker(1, generation, true); value != 0 || complete {
		t.Fatalf("overflow was not fail-closed: (%d, %v)", value, complete)
	}
	if value, complete := p3ACCUpdateDataRootPhysicalTracker(2, generation, true); value != 0 || complete {
		t.Fatalf("overflow poison was implicitly reset: (%d, %v)", value, complete)
	}
	resetP3ACCDataRootPhysicalTracker()
	if value, complete := p3ACCUpdateDataRootPhysicalTracker(2, generation, true); value != 0 || complete {
		t.Fatalf("explicit reset did not establish a new baseline: (%d, %v)", value, complete)
	}
	if value, complete := p3ACCUpdateDataRootPhysicalTracker(3, generation, true); value != 1 || !complete {
		t.Fatalf("tracker did not recover after explicit reset: (%d, %v)", value, complete)
	}
}

func TestP3ACCDataRootWalkerSumOverflowPoisonsTrackerUntilReset(t *testing.T) {
	resetP3ACCDataRootPhysicalTracker()
	defer resetP3ACCDataRootPhysicalTracker()
	total := int64(math.MaxInt64)
	status := p3ACCAddPhysicalBytesStatus(&total, 1)
	if status != p3ACCDataRootSampleInvalid || total != math.MaxInt64 {
		t.Fatalf("walker sum overflow = (status %d, total %d)", status, total)
	}
	generation := p3ACCDataRootTestGeneration(1)
	if value, complete := p3ACCUpdateDataRootPhysicalTrackerStatus(
		100, generation, p3ACCDataRootSampleComplete,
	); value != 0 || complete {
		t.Fatalf("initial complete sample = (%d, %v)", value, complete)
	}
	if value, complete := p3ACCUpdateDataRootPhysicalTrackerStatus(
		0, generation, status,
	); value != 0 || complete {
		t.Fatalf("walker sum overflow was not fail-closed: (%d, %v)", value, complete)
	}
	if value, complete := p3ACCUpdateDataRootPhysicalTrackerStatus(
		120, generation, p3ACCDataRootSampleComplete,
	); value != 0 || complete {
		t.Fatalf("walker sum overflow poison was implicitly reset: (%d, %v)", value, complete)
	}
	resetP3ACCDataRootPhysicalTracker()
	if value, complete := p3ACCUpdateDataRootPhysicalTracker(120, generation, true); value != 0 || complete {
		t.Fatalf("explicit reset did not establish a new baseline: (%d, %v)", value, complete)
	}
	if value, complete := p3ACCUpdateDataRootPhysicalTracker(121, generation, true); value != 1 || !complete {
		t.Fatalf("tracker did not recover after reset: (%d, %v)", value, complete)
	}
}

func p3ACCDataRootTestAppendAndSync(file *os.File, payload []byte) error {
	if file == nil {
		return os.ErrInvalid
	}
	if _, err := file.Write(payload); err != nil {
		return err
	}
	return file.Sync()
}

func TestP3ACCDataRootWalkerAcceptsActiveFFmpegAndWALGrowth(t *testing.T) {
	for _, name := range []string{"recording-000.ts", "douyin_live.db-wal"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			filename := filepath.Join(root, name)
			initial := make([]byte, 4096)
			for index := range initial {
				initial[index] = byte(index%251 + 1)
			}
			if err := os.WriteFile(filename, initial, 0o600); err != nil {
				t.Fatalf("write active-file fixture: %v", err)
			}
			before, err := os.Stat(filename)
			if err != nil {
				t.Fatalf("stat active-file fixture: %v", err)
			}
			writer, err := os.OpenFile(filename, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				t.Fatalf("open persistent active-file writer: %v", err)
			}
			writerClosed := false
			defer func() {
				if !writerClosed {
					_ = writer.Close()
				}
			}()
			baselineAllocation, baselineComplete := p3ACCWalkDataRootPhysicalBytes(root)
			if !baselineComplete || baselineAllocation <= 0 {
				t.Fatalf("active-file baseline allocation = (%d, %v)", baselineAllocation, baselineComplete)
			}
			originalEnumerationHook := p3ACCDataRootAfterFileEnumeration
			originalOpenHook := p3ACCDataRootAfterFileOpen
			originalFirstStandardHook := p3ACCDataRootAfterFirstStandard
			defer func() {
				p3ACCDataRootAfterFileEnumeration = originalEnumerationHook
				p3ACCDataRootAfterFileOpen = originalOpenHook
				p3ACCDataRootAfterFirstStandard = originalFirstStandardHook
			}()
			growth := make([]byte, 64*1024)
			for index := range growth {
				growth[index] = byte(index%239 + 1)
			}
			growthCalls := 0
			growConcurrently := func(candidate string) {
				if !strings.EqualFold(candidate, filename) {
					t.Fatalf("growth hook escaped fixture: %s", candidate)
				}
				growthCalls++
				done := make(chan error, 1)
				go func() {
					done <- p3ACCDataRootTestAppendAndSync(writer, growth)
				}()
				if err := <-done; err != nil {
					t.Fatalf("append active-file fixture: %v", err)
				}
			}
			p3ACCDataRootAfterFileEnumeration = growConcurrently
			p3ACCDataRootAfterFileOpen = growConcurrently
			p3ACCDataRootAfterFirstStandard = growConcurrently
			physicalBytes, complete := p3ACCWalkDataRootPhysicalBytes(root)
			if !complete || physicalBytes <= baselineAllocation {
				t.Fatalf("active-file allocation = (%d, %v), baseline %d", physicalBytes, complete, baselineAllocation)
			}
			if growthCalls != 3 {
				t.Fatalf("active-file growth hooks = %d, want 3", growthCalls)
			}
			after, err := os.Stat(filename)
			if err != nil || after.Size() <= before.Size() {
				t.Fatalf("active file did not grow: before=%d after=%v err=%v", before.Size(), after, err)
			}
			if err := writer.Close(); err != nil {
				t.Fatalf("close persistent active-file writer: %v", err)
			}
			writerClosed = true
		})
	}
}

func TestP3ACCDataRootTrackerRejectsSamePathRootGenerationABAUntilReset(t *testing.T) {
	resetP3ACCDataRootPhysicalTracker()
	defer resetP3ACCDataRootPhysicalTracker()
	container := t.TempDir()
	root := filepath.Join(container, "data")
	retired := filepath.Join(container, "data-retired")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatalf("create first root generation: %v", err)
	}
	oldFile := filepath.Join(root, "old.bin")
	if err := os.WriteFile(oldFile, make([]byte, 4096), 0o600); err != nil {
		t.Fatalf("write first root generation: %v", err)
	}
	_, oldGeneration, oldStatus := p3ACCWalkDataRootPhysicalSample(root)
	if oldStatus != p3ACCDataRootSampleComplete || !p3ACCValidDataRootGeneration(oldGeneration) {
		t.Fatalf("first root generation = (%#v, %d)", oldGeneration, oldStatus)
	}
	if value, complete := readP3ACCDataRootPhysicalSample(root); value != 0 || complete {
		t.Fatalf("first root baseline = (%d, %v)", value, complete)
	}

	if err := os.Rename(root, retired); err != nil {
		t.Fatalf("retire first root generation: %v", err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatalf("create second root generation: %v", err)
	}
	newFile := filepath.Join(root, "new.bin")
	if err := os.WriteFile(newFile, make([]byte, 128*1024), 0o600); err != nil {
		t.Fatalf("write second root generation: %v", err)
	}
	_, newGeneration, newStatus := p3ACCWalkDataRootPhysicalSample(root)
	if newStatus != p3ACCDataRootSampleComplete || !p3ACCValidDataRootGeneration(newGeneration) ||
		newGeneration == oldGeneration {
		t.Fatalf("same-path root generations = old %#v, new %#v, status %d", oldGeneration, newGeneration, newStatus)
	}
	if value, complete := readP3ACCDataRootPhysicalSample(root); value != 0 || complete {
		t.Fatalf("same-path root ABA was accepted: (%d, %v)", value, complete)
	}
	if value, complete := readP3ACCDataRootPhysicalSample(root); value != 0 || complete {
		t.Fatalf("root-generation poison was implicitly reset: (%d, %v)", value, complete)
	}

	resetP3ACCDataRootPhysicalTracker()
	if value, complete := readP3ACCDataRootPhysicalSample(root); value != 0 || complete {
		t.Fatalf("reset root-generation baseline = (%d, %v)", value, complete)
	}
	writer, err := os.OpenFile(newFile, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatalf("open second-generation writer: %v", err)
	}
	if err := p3ACCDataRootTestAppendAndSync(writer, make([]byte, 128*1024)); err != nil {
		_ = writer.Close()
		t.Fatalf("grow second root generation: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close second-generation writer: %v", err)
	}
	if value, complete := readP3ACCDataRootPhysicalSample(root); value <= 0 || !complete {
		t.Fatalf("tracker did not recover after explicit root-generation reset: (%d, %v)", value, complete)
	}
}

func TestP3ACCDataRootWalkerRejectsDeletePendingFileAsTransient(t *testing.T) {
	root := t.TempDir()
	filename := filepath.Join(root, "delete-pending.ts")
	if err := os.WriteFile(filename, make([]byte, 4096), 0o600); err != nil {
		t.Fatalf("write delete-pending fixture: %v", err)
	}
	originalHook := p3ACCDataRootAfterFirstStandard
	defer func() { p3ACCDataRootAfterFirstStandard = originalHook }()
	removed := false
	p3ACCDataRootAfterFirstStandard = func(candidate string) {
		if !strings.EqualFold(candidate, filename) {
			t.Fatalf("delete-pending hook escaped fixture: %s", candidate)
		}
		if err := os.Remove(candidate); err != nil {
			t.Fatalf("mark held file delete-pending: %v", err)
		}
		removed = true
	}
	if _, _, status := p3ACCWalkDataRootPhysicalSample(root); status != p3ACCDataRootSampleTransient {
		t.Fatalf("delete-pending sample status = %d", status)
	}
	if !removed {
		t.Fatal("delete-pending hook did not run")
	}
	if _, err := os.Lstat(filename); !os.IsNotExist(err) {
		t.Fatalf("delete-pending fixture still resolves: %v", err)
	}
	p3ACCDataRootAfterFirstStandard = originalHook
	if err := os.WriteFile(filename, make([]byte, 4096), 0o600); err != nil {
		t.Fatalf("recreate delete-pending fixture: %v", err)
	}
	if _, complete := p3ACCWalkDataRootPhysicalBytes(root); !complete {
		t.Fatal("walker did not recover after delete-pending race")
	}
}

func TestP3ACCDataRootReadSerializesResetWhileWalkIsBlocked(t *testing.T) {
	resetP3ACCDataRootPhysicalTracker()
	defer resetP3ACCDataRootPhysicalTracker()
	root := t.TempDir()
	filename := filepath.Join(root, "active.ts")
	if err := os.WriteFile(filename, make([]byte, 4096), 0o600); err != nil {
		t.Fatalf("write serialized-sample fixture: %v", err)
	}
	originalHook := p3ACCDataRootAfterFileEnumeration
	defer func() { p3ACCDataRootAfterFileEnumeration = originalHook }()
	entered := make(chan string, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseWalk := func() { releaseOnce.Do(func() { close(release) }) }
	defer releaseWalk()
	p3ACCDataRootAfterFileEnumeration = func(candidate string) {
		entered <- candidate
		<-release
	}
	type sampleResult struct {
		value    int64
		complete bool
	}
	readDone := make(chan sampleResult, 1)
	go func() {
		value, complete := readP3ACCDataRootPhysicalSample(root)
		readDone <- sampleResult{value: value, complete: complete}
	}()
	select {
	case candidate := <-entered:
		if !strings.EqualFold(candidate, filename) {
			releaseWalk()
			t.Fatalf("serialized-sample hook escaped fixture: %s", candidate)
		}
	case <-time.After(2 * time.Second):
		releaseWalk()
		t.Fatal("read did not reach the blocking walk hook")
	}
	if p3ACCDataRootPhysicalSampleMu.TryLock() {
		p3ACCDataRootPhysicalSampleMu.Unlock()
		releaseWalk()
		t.Fatal("read did not hold the sample mutex while blocked in the walk")
	}
	resetStarted := make(chan struct{})
	resetDone := make(chan struct{})
	go func() {
		close(resetStarted)
		resetP3ACCDataRootPhysicalTracker()
		close(resetDone)
	}()
	<-resetStarted
	select {
	case <-resetDone:
		releaseWalk()
		t.Fatal("reset returned before the blocked read released the sample mutex")
	case <-time.After(50 * time.Millisecond):
	}
	releaseWalk()
	select {
	case result := <-readDone:
		if result.value != 0 || result.complete {
			t.Fatalf("serialized first sample = (%d, %v)", result.value, result.complete)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked read did not finish after release")
	}
	select {
	case <-resetDone:
	case <-time.After(2 * time.Second):
		t.Fatal("reset did not finish after the read released the sample mutex")
	}
	p3ACCDataRootPhysicalTracker.Lock()
	initialized := p3ACCDataRootPhysicalTracker.initialized
	poisoned := p3ACCDataRootPhysicalTracker.poisoned
	previous := p3ACCDataRootPhysicalTracker.previous
	cumulative := p3ACCDataRootPhysicalTracker.cumulative
	generation := p3ACCDataRootPhysicalTracker.generation
	p3ACCDataRootPhysicalTracker.Unlock()
	if initialized || poisoned || previous != 0 || cumulative != 0 ||
		generation != (p3ACCDataRootGeneration{}) {
		t.Fatalf(
			"post-reset tracker = initialized %v poisoned %v previous %d cumulative %d generation %#v",
			initialized, poisoned, previous, cumulative, generation,
		)
	}
}

func TestP3ACCDataRootWalkerRejectsShrinkAndReplacementRaces(t *testing.T) {
	t.Run("shrink", func(t *testing.T) {
		root := t.TempDir()
		filename := filepath.Join(root, "active.ts")
		if err := os.WriteFile(filename, make([]byte, 64*1024), 0o600); err != nil {
			t.Fatalf("write shrink fixture: %v", err)
		}
		originalHook := p3ACCDataRootAfterFileEnumeration
		defer func() { p3ACCDataRootAfterFileEnumeration = originalHook }()
		p3ACCDataRootAfterFileEnumeration = func(candidate string) {
			if err := os.Truncate(candidate, 1024); err != nil {
				t.Fatalf("truncate active fixture: %v", err)
			}
		}
		if _, _, status := p3ACCWalkDataRootPhysicalSample(root); status != p3ACCDataRootSampleTransient {
			t.Fatalf("shrink race status = %d", status)
		}
		p3ACCDataRootAfterFileEnumeration = originalHook
		if _, complete := p3ACCWalkDataRootPhysicalBytes(root); !complete {
			t.Fatal("walker did not recover after a transient shrink race")
		}
	})

	t.Run("shrink_between_standard_samples", func(t *testing.T) {
		root := t.TempDir()
		filename := filepath.Join(root, "active.ts")
		if err := os.WriteFile(filename, make([]byte, 64*1024), 0o600); err != nil {
			t.Fatalf("write shrink fixture: %v", err)
		}
		originalHook := p3ACCDataRootAfterFirstStandard
		defer func() { p3ACCDataRootAfterFirstStandard = originalHook }()
		p3ACCDataRootAfterFirstStandard = func(candidate string) {
			if !strings.EqualFold(candidate, filename) {
				t.Fatalf("shrink hook escaped fixture: %s", candidate)
			}
			if err := os.Truncate(candidate, 1024); err != nil {
				t.Fatalf("truncate active fixture: %v", err)
			}
		}
		if _, _, status := p3ACCWalkDataRootPhysicalSample(root); status != p3ACCDataRootSampleTransient {
			t.Fatalf("between-standard shrink status = %d", status)
		}
		p3ACCDataRootAfterFirstStandard = originalHook
		if _, complete := p3ACCWalkDataRootPhysicalBytes(root); !complete {
			t.Fatal("walker did not recover after a between-standard shrink race")
		}
	})

	t.Run("replacement", func(t *testing.T) {
		root := t.TempDir()
		filename := filepath.Join(root, "active.db-wal")
		retired := filepath.Join(root, "retired.db-wal")
		if err := os.WriteFile(filename, make([]byte, 4096), 0o600); err != nil {
			t.Fatalf("write replacement fixture: %v", err)
		}
		originalHook := p3ACCDataRootAfterSecondStandard
		defer func() { p3ACCDataRootAfterSecondStandard = originalHook }()
		replaced := false
		p3ACCDataRootAfterSecondStandard = func(candidate string) {
			if replaced {
				return
			}
			replaced = true
			if err := os.Rename(candidate, retired); err != nil {
				t.Fatalf("retire held file: %v", err)
			}
			if err := os.WriteFile(candidate, make([]byte, 8192), 0o600); err != nil {
				t.Fatalf("replace held file: %v", err)
			}
		}
		if _, _, status := p3ACCWalkDataRootPhysicalSample(root); status != p3ACCDataRootSampleTransient {
			t.Fatalf("replacement race status = %d", status)
		}
		p3ACCDataRootAfterSecondStandard = originalHook
		if _, complete := p3ACCWalkDataRootPhysicalBytes(root); !complete {
			t.Fatal("walker did not recover after a transient replacement race")
		}
	})
}

func TestP3ACCDataRootWalkerIsBoundedIdentitySafeAndReparseSafe(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "one.bin"), []byte("one"), 0o600); err != nil {
		t.Fatalf("write root fixture: %v", err)
	}
	subdirectory := filepath.Join(root, "sub")
	if err := os.Mkdir(subdirectory, 0o700); err != nil {
		t.Fatalf("create subdirectory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(subdirectory, "two.bin"), []byte("four"), 0o600); err != nil {
		t.Fatalf("write nested fixture: %v", err)
	}
	if value, complete := p3ACCWalkDataRootPhysicalBytes(root); value < 7 || !complete {
		t.Fatalf("regular allocated footprint = (%d, %v)", value, complete)
	}
	uncleanRoot := root + string(filepath.Separator) + "."
	if _, complete := p3ACCWalkDataRootPhysicalBytes(uncleanRoot); complete {
		t.Fatal("non-canonical data root was accepted")
	}
	outside := t.TempDir()
	outsideState := &p3ACCDataRootWalkState{
		dataRoot:       root,
		maximumEntries: p3ACCDataRootMaximumEntries,
		seen:           make(map[p3ACCDataRootFileIdentity]struct{}),
	}
	if _, complete := p3ACCWalkDataRootDirectory(outside, nil, 0, outsideState); complete {
		t.Fatal("directory outside the canonical data root was accepted")
	}
	state := &p3ACCDataRootWalkState{
		entryCount:     p3ACCDataRootMaximumEntries,
		maximumEntries: p3ACCDataRootMaximumEntries,
		seen:           make(map[p3ACCDataRootFileIdentity]struct{}),
	}
	if _, complete := p3ACCWalkDataRootDirectory(root, nil, 0, state); complete {
		t.Fatal("entry cap was not fail-closed")
	}
	total := int64(math.MaxInt64)
	if p3ACCAddPhysicalBytes(&total, 1) {
		t.Fatal("footprint sum overflow was accepted")
	}

	limitedRoot := t.TempDir()
	for index := 0; index < 4; index++ {
		filename := filepath.Join(limitedRoot, fmt.Sprintf("entry-%d.bin", index))
		if err := os.WriteFile(filename, []byte{byte(index)}, 0o600); err != nil {
			t.Fatalf("write bounded-enumeration fixture: %v", err)
		}
	}
	limitedState := &p3ACCDataRootWalkState{
		maximumEntries: 3,
		seen:           make(map[p3ACCDataRootFileIdentity]struct{}),
	}
	if _, complete := p3ACCWalkDataRootDirectory(limitedRoot, nil, 0, limitedState); complete {
		t.Fatal("single directory above injected entry cap was accepted")
	}
	if limitedState.entryCount != limitedState.maximumEntries {
		t.Fatalf(
			"bounded enumeration consumed %d entries, want %d",
			limitedState.entryCount,
			limitedState.maximumEntries,
		)
	}

	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "escaped.bin"), []byte("escape"), 0o600); err != nil {
		t.Fatalf("write junction target: %v", err)
	}
	junction := filepath.Join(root, "junction")
	if err := exec.Command("cmd.exe", "/d", "/c", "mklink", "/J", junction, target).Run(); err != nil {
		t.Fatalf("create junction: %v", err)
	}
	if _, complete := p3ACCWalkDataRootPhysicalBytes(root); complete {
		t.Fatal("data-root junction was accepted")
	}

	symlinkRoot := t.TempDir()
	symlinkTarget := filepath.Join(t.TempDir(), "target.bin")
	if err := os.WriteFile(symlinkTarget, []byte("target"), 0o600); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}
	symlink := filepath.Join(symlinkRoot, "linked.bin")
	if err := os.Symlink(symlinkTarget, symlink); err != nil {
		t.Logf("file symlink capability unavailable: %v", err)
	} else if _, complete := p3ACCWalkDataRootPhysicalBytes(symlinkRoot); complete {
		t.Fatal("data-root file symlink was accepted")
	}

	hardRoot := t.TempDir()
	original := filepath.Join(t.TempDir(), "original.bin")
	linked := filepath.Join(hardRoot, "linked.bin")
	if err := os.WriteFile(original, []byte("hard-link"), 0o600); err != nil {
		t.Fatalf("write hard-link fixture: %v", err)
	}
	if err := os.Link(original, linked); err != nil {
		t.Fatalf("create hard link: %v", err)
	}
	if _, complete := p3ACCWalkDataRootPhysicalBytes(hardRoot); complete {
		t.Fatal("single in-root path with an external hard link was accepted")
	}
}

func TestP3ACCProcessSnapshotTakesCutoffBeforeRealSnapshotEnumeration(t *testing.T) {
	originalTime := p3ACCGetSystemTimeAsFileTime
	originalCreate := p3ACCCreateToolhelp32Snapshot
	originalFirst := p3ACCProcess32First
	defer func() {
		p3ACCGetSystemTimeAsFileTime = originalTime
		p3ACCCreateToolhelp32Snapshot = originalCreate
		p3ACCProcess32First = originalFirst
	}()
	order := make([]string, 0, 3)
	p3ACCGetSystemTimeAsFileTime = func(value *windows.Filetime) {
		order = append(order, "cutoff")
		originalTime(value)
	}
	p3ACCCreateToolhelp32Snapshot = func(flags uint32, processID uint32) (windows.Handle, error) {
		order = append(order, "snapshot")
		return originalCreate(flags, processID)
	}
	p3ACCProcess32First = func(snapshot windows.Handle, entry *windows.ProcessEntry32) error {
		order = append(order, "first")
		return originalFirst(snapshot, entry)
	}
	if _, _, complete := p3ACCCaptureProcessSnapshot(); !complete {
		t.Fatal("instrumented real process snapshot failed")
	}
	if len(order) < 3 || order[0] != "cutoff" || order[1] != "snapshot" || order[2] != "first" {
		t.Fatalf("process snapshot order = %v", order)
	}
}

func TestP3ACCProcessIdentityRequiresCutoffParentAndCreationMatch(t *testing.T) {
	entries, cutoff, complete := p3ACCCaptureProcessSnapshot()
	if !complete {
		t.Fatal("capture process snapshot failed")
	}
	processID := uint32(os.Getpid())
	entry, exists := entries[processID]
	if !exists {
		t.Fatal("current process absent from snapshot")
	}
	process, err := windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE,
		false,
		processID,
	)
	if err != nil {
		t.Fatalf("open current process: %v", err)
	}
	defer windows.CloseHandle(process)
	createdAt, timesOK := p3ACCProcessCreatedAt(process)
	identity := p3ACCProcessIdentity{processID: processID, createdAt: createdAt}
	if !timesOK || !p3ACCVerifyProcessIdentity(
		process, identity, entry.ParentProcessID, cutoff, entries,
	) {
		t.Fatal("stable current-process identity was rejected")
	}
	if p3ACCVerifyProcessIdentity(
		process, identity, entry.ParentProcessID+1, cutoff, entries,
	) {
		t.Fatal("wrong parent identity was accepted")
	}
	if createdAt > 0 && p3ACCVerifyProcessIdentity(
		process, identity, entry.ParentProcessID, createdAt-1, entries,
	) {
		t.Fatal("post-snapshot creation time was accepted")
	}
}
