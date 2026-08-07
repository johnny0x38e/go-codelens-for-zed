package analysis

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

func FuzzEngineMatchesFullAnalysis(f *testing.F) {
	f.Add(uint8(0), uint8(1))
	f.Add(uint8(1), uint8(4))
	f.Add(uint8(2), uint8(7))

	f.Fuzz(func(t *testing.T, seed, editCount uint8) {
		packageCount := 1 + int(seed%6)
		stepCount := 1 + int(editCount%8)
		moduleDir := t.TempDir()
		writeAnalysisTestFile(t, filepath.Join(moduleDir, "go.mod"), "module example.com/fuzz\n\ngo 1.26\n")

		paths := make([]string, 0, packageCount)
		for packageIndex := range packageCount {
			path := filepath.Join(moduleDir, fmt.Sprintf("pkg%d", packageIndex), "sample.go")
			writeAnalysisTestFile(t, path, fuzzSource(seed, packageIndex, 0))
			paths = append(paths, path)
		}

		scope := ModuleAnalysisScope(moduleDir)
		engine := NewEngine()
		if _, err := engine.Analyze(context.Background(), scope, ChangeSet{}, nil); err != nil {
			t.Fatalf("prime Engine.Analyze() error = %v", err)
		}

		overlays := make(map[string][]byte, packageCount)
		for step := 1; step <= stepCount; step++ {
			packageIndex := (step - 1) % packageCount
			path := paths[packageIndex]
			overlays[path] = []byte(fuzzSource(seed, packageIndex, step))
			got, err := engine.Analyze(
				context.Background(),
				scope,
				NewChangeSet(Change{Path: path, Kind: ChangeContent}),
				overlays,
			)
			if err != nil {
				t.Fatalf("step %d Engine.Analyze() error = %v", step, err)
			}
			want, err := AnalyzeScope(context.Background(), scope, overlays)
			if err != nil {
				t.Fatalf("step %d AnalyzeScope() error = %v", step, err)
			}
			assertSnapshotsEqual(t, got, want, paths)
		}
	})
}

func fuzzSource(seed uint8, packageIndex, revision int) string {
	prefixes := [...]string{"Worker", "DeltaWorker", "U00010400Worker"}
	typeName := prefixes[(int(seed)+packageIndex)%len(prefixes)]
	methodReceiver := typeName
	familyReceiver := "*" + typeName
	implementationValue := "value"
	if revision%2 != 0 {
		methodReceiver, familyReceiver = familyReceiver, methodReceiver
		implementationValue = "&value"
	}
	constraint := "~int"
	if revision%3 == 0 {
		constraint = "~int | ~int64"
	}

	return fmt.Sprintf(`package pkg%d

type First interface { Method() int }
type Second interface { Method() int }
type Combined interface {
	First
	Second
}
type Family interface { FamilyMethod() int }

type %s struct{}

func (%s) Method() int { return %d }
func (%s) FamilyMethod() int { return %d }

func Identity[T %s](value T) T { return value }

func Use() int {
	var value %s
	var first First = %s
	var second Second = %s
	var combined Combined = %s
	var family Family = &value
	return first.Method() + second.Method() + combined.Method() + family.FamilyMethod() + Identity(%d)
}
`,
		packageIndex,
		typeName,
		methodReceiver,
		revision,
		familyReceiver,
		revision+1,
		constraint,
		typeName,
		implementationValue,
		implementationValue,
		implementationValue,
		revision,
	)
}
