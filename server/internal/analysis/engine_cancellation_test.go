package analysis

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestEngineCanceledSuccessCannotPublish(t *testing.T) {
	for _, primed := range []bool{false, true} {
		name := "initial"
		if primed {
			name = "retained"
		}
		t.Run(name, func(t *testing.T) {
			scope := ModuleAnalysisScope(t.TempDir())
			engine := NewEngine()
			before := newSnapshot(map[string][]Lens{"before": {{Title: "before"}}})
			engine.analyze = func(context.Context, AnalysisScope, map[string][]byte) (scopeAnalysis, error) {
				return scopeAnalysis{snapshot: before}, nil
			}
			if primed {
				if _, err := engine.Analyze(t.Context(), scope, ChangeSet{}, nil); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			engine.analyze = func(context.Context, AnalysisScope, map[string][]byte) (scopeAnalysis, error) {
				// Model a lower-level operation that returns success after its
				// caller has already canceled the request.
				cancel()
				return scopeAnalysis{snapshot: newSnapshot(map[string][]Lens{"stale": {{Title: "stale"}}})}, nil
			}
			if _, err := engine.Analyze(
				ctx,
				scope,
				NewChangeSet(Change{Kind: ChangeMetadata}),
				nil,
			); !errors.Is(
				err,
				context.Canceled,
			) {
				t.Fatalf("canceled successful analyzer returned %v", err)
			}
			state, retained := engine.states[scope.CacheKey()]
			if primed {
				if !retained || !reflect.DeepEqual(state.snapshot, before) {
					t.Fatal("canceled analysis replaced successful state")
				}
			} else if retained || len(engine.scopeControls) != 0 {
				t.Fatal("canceled initial analysis retained state or its publication token")
			}
		})
	}
}

func TestEngineCanceledCacheHitReturnsCancellation(t *testing.T) {
	engine := NewEngine()
	scope := ModuleAnalysisScope(t.TempDir())
	engine.analyze = func(context.Context, AnalysisScope, map[string][]byte) (scopeAnalysis, error) {
		return scopeAnalysis{}, nil
	}
	if _, err := engine.Analyze(t.Context(), scope, ChangeSet{}, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := engine.Analyze(ctx, scope, ChangeSet{}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled cache hit returned %v", err)
	}
}
