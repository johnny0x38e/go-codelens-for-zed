package lsp

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/johnny0x38e/go-codelens-for-zed/server/internal/analysis"
)

func TestSnapshotAnalysisConcurrencyIsBounded(t *testing.T) {
	for _, sameScope := range []bool{true, false} {
		name := "scopes"
		if sameScope {
			name = "revisions"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			state := newServerStateWithContext(ctx)
			root := t.TempDir()
			const flightCount = maxSnapshotCacheEntries + 2
			started := make(chan struct{}, flightCount)
			release := make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			var calls atomic.Int64
			state.analyzeScope = func(
				analysisCtx context.Context,
				_ analysis.AnalysisScope,
				_ analysis.ChangeSet,
				_ map[string][]byte,
			) (analysis.Snapshot, error) {
				calls.Add(1)
				started <- struct{}{}
				// Model a parser or checker that cannot stop in the middle of its
				// current operation even after its revision has been canceled.
				<-release
				return analysis.Snapshot{}, analysisCtx.Err()
			}

			flights := make([]*analysisFlight, 0, flightCount)
			for index := range flightCount {
				scope := analysis.ModuleAnalysisScope(filepath.Join(root, "module"))
				if !sameScope {
					scope = analysis.ModuleAnalysisScope(filepath.Join(root, strconv.Itoa(index)))
				} else if index > 0 {
					state.invalidateSnapshotsForPath(filepath.Join(scope.Root, "edited.go"))
				}
				_, flight, cached := state.snapshotOrFlight(scope)
				if cached || flight == nil {
					t.Fatalf("flight %d was not created", index)
				}
				flights = append(flights, flight)
				if index == 0 {
					select {
					case <-started:
					case <-ctx.Done():
						t.Fatal("first analysis did not start")
					}
				}
			}

			select {
			case <-started:
				t.Fatal("a new analysis started while a canceled analysis was still running")
			case <-time.After(50 * time.Millisecond):
			}
			unblock()
			for _, flight := range flights {
				select {
				case <-flight.done:
					_, err := state.consumeAnalysisFlight(flight)
					if err != nil && !errors.Is(err, errAnalysisObsolete) {
						t.Fatalf("flight result = %v, want success or obsolete revision", err)
					}
				case <-ctx.Done():
					t.Fatal("analysis did not finish after releasing the old work")
				}
			}
			if sameScope && calls.Load() != 2 {
				t.Fatalf("analysis calls = %d, want only the first and latest revisions", calls.Load())
			}
		})
	}
}

func TestQueuedAnalysisCancellationDoesNotWaitForRunningChecker(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	state := newServerStateWithContext(ctx)
	root := t.TempDir()
	first := analysis.ModuleAnalysisScope(filepath.Join(root, "first"))
	second := analysis.ModuleAnalysisScope(filepath.Join(root, "second"))
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var calls atomic.Int64
	state.analyzeScope = func(context.Context, analysis.AnalysisScope, analysis.ChangeSet, map[string][]byte) (analysis.Snapshot, error) {
		calls.Add(1)
		started <- struct{}{}
		<-release
		return analysis.Snapshot{}, nil
	}
	_, firstFlight, _ := state.snapshotOrFlight(first)
	<-started
	requestCtx, cancelRequest := context.WithCancel(ctx)
	result := make(chan error, 1)
	go func() {
		_, err := state.snapshotForScope(requestCtx, second)
		result <- err
	}()
	var queued *analysisFlight
	for queued == nil && ctx.Err() == nil {
		state.cacheMu.RLock()
		queued = state.analysisFlights[second.CacheKey()]
		state.cacheMu.RUnlock()
		runtime.Gosched()
	}
	if queued == nil {
		t.Fatal("request did not enter the analysis queue")
	}
	cancelRequest()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("queued request returned %v, want context.Canceled", err)
		}
	case <-ctx.Done():
		t.Fatal("queued request cancellation waited for the running checker")
	}
	select {
	case <-queued.done:
	case <-ctx.Done():
		t.Fatal("canceled queue entry did not finish independently")
	}
	if calls.Load() != 1 {
		t.Fatal("canceled queued request entered the analyzer")
	}
	unblock()
	<-firstFlight.done
	_, _ = state.consumeAnalysisFlight(firstFlight)
	if _, err := state.snapshotForScope(ctx, second); err != nil || calls.Load() != 2 {
		t.Fatalf("replacement request = %v after %d analyses", err, calls.Load())
	}
}

func BenchmarkCancelQueuedAnalysis(b *testing.B) {
	state := newServerStateWithContext(b.Context())
	scope := analysis.ModuleAnalysisScope(b.TempDir())
	key := scope.CacheKey()
	// Hold the sole slot as though a non-interruptible checker still owns it.
	state.analysisSlot <- struct{}{}
	defer func() { <-state.analysisSlot }()
	state.analyzeScope = func(context.Context, analysis.AnalysisScope, analysis.ChangeSet, map[string][]byte) (analysis.Snapshot, error) {
		b.Error("canceled queued work entered the semantic analyzer")
		return analysis.Snapshot{}, nil
	}
	b.ReportAllocs()
	for b.Loop() {
		_, flight, cached := state.snapshotOrFlight(scope)
		if cached {
			b.Fatal("queued work unexpectedly produced a cached snapshot")
		}
		state.releaseAnalysisWaiter(key, flight)
		<-flight.done
	}
}

func BenchmarkSnapshotEditBurst(b *testing.B) {
	ctx := b.Context()
	scope := analysis.ModuleAnalysisScope(b.TempDir())
	changedPath := filepath.Join(scope.Root, "edited.go")
	const revisions = 16
	var peak int64

	b.ReportAllocs()
	for b.Loop() {
		state := newServerStateWithContext(ctx)
		started := make(chan struct{}, revisions)
		release := make(chan struct{})
		var active atomic.Int64
		var iterationPeak atomic.Int64
		state.analyzeScope = func(
			analysisCtx context.Context,
			_ analysis.AnalysisScope,
			_ analysis.ChangeSet,
			_ map[string][]byte,
		) (analysis.Snapshot, error) {
			// A fixed working set makes obsolete overlapping graphs visible in
			// B/op without loading a real workspace or depending on GC timing.
			scratch := make([]byte, 1<<20)
			count := active.Add(1)
			for previous := iterationPeak.Load(); count > previous; previous = iterationPeak.Load() {
				if iterationPeak.CompareAndSwap(previous, count) {
					break
				}
			}
			started <- struct{}{}
			<-release
			runtime.KeepAlive(scratch)
			active.Add(-1)
			return analysis.Snapshot{}, analysisCtx.Err()
		}

		flights := make([]*analysisFlight, 0, revisions)
		for revision := range revisions {
			if revision > 0 {
				state.invalidateSnapshotsForPath(changedPath)
			}
			_, flight, _ := state.snapshotOrFlight(scope)
			flights = append(flights, flight)
			if revision == 0 {
				<-started
			}
			// Model a rapid edit stream while the first checker is still busy.
			time.Sleep(time.Millisecond)
		}
		close(release)
		for _, flight := range flights {
			<-flight.done
			_, err := state.consumeAnalysisFlight(flight)
			if err != nil && !errors.Is(err, errAnalysisObsolete) {
				b.Fatalf("flight result = %v", err)
			}
		}
		peak = max(peak, iterationPeak.Load())
	}
	b.ReportMetric(float64(peak), "peak-analyses")
}
