package analysis

import (
	"context"
	"path/filepath"
	"testing"
)

func TestEngineMatchesFullAnalysisAcrossSemanticEditSequence(t *testing.T) {
	moduleDir := t.TempDir()
	writeAnalysisTestFile(t, filepath.Join(moduleDir, "go.mod"), "module example.com/sequence\n\ngo 1.26\n")
	contractFile := filepath.Join(moduleDir, "contract.go")
	implementationFile := filepath.Join(moduleDir, "implementation.go")
	internalTestFile := filepath.Join(moduleDir, "sequence_test.go")
	externalTestFile := filepath.Join(moduleDir, "external_test.go")

	contract := `package sequence

type Base interface {
	Base() string
}

type Runner interface {
	Base
	Run(int) string
}

func Identity[T ~int](value T) T { return value }

func Use(runner Runner) string {
	return runner.Run(Identity(1)) + runner.Base()
}
`
	implementation := `package sequence

type Worker struct{}

func (Worker) Base() string { return "base" }
func (Worker) Run(int) string { return "run" }

func Invoke() string { return Use(Worker{}) }
`
	internalTest := `package sequence

import "testing"

func TestInvoke(t *testing.T) {
	if Invoke() == "" {
		t.Fatal("empty result")
	}
}
`
	externalTest := `package sequence_test

import (
	"testing"

	"example.com/sequence"
)

func TestPublicAPI(t *testing.T) {
	if sequence.Invoke() == "" {
		t.Fatal("empty result")
	}
}
`
	writeAnalysisTestFile(t, contractFile, contract)
	writeAnalysisTestFile(t, implementationFile, implementation)
	writeAnalysisTestFile(t, internalTestFile, internalTest)
	writeAnalysisTestFile(t, externalTestFile, externalTest)

	type editStep struct {
		name    string
		updates map[string]string
	}
	steps := []editStep{
		{
			name: "body reference",
			updates: map[string]string{
				implementationFile: `package sequence

type Worker struct{}

func (Worker) Base() string { return "base" }
func (Worker) Run(int) string { return "run" }

func Invoke() string { return Use(Worker{}) + Worker{}.Base() }
`,
			},
		},
		{
			name: "receiver pointer",
			updates: map[string]string{
				implementationFile: `package sequence

type Worker struct{}

func (Worker) Base() string { return "base" }
func (*Worker) Run(int) string { return "run" }

func Invoke() string { return Use(&Worker{}) + Worker{}.Base() }
`,
			},
		},
		{
			name: "interface method",
			updates: map[string]string{
				contractFile: `package sequence

type Base interface {
	Base() string
}

type Runner interface {
	Base
	Run(int) string
	Stop() string
}

func Identity[T ~int](value T) T { return value }

func Use(runner Runner) string {
	return runner.Run(Identity(1)) + runner.Base() + runner.Stop()
}
`,
				implementationFile: `package sequence

type Worker struct{}

func (Worker) Base() string { return "base" }
func (*Worker) Run(int) string { return "run" }
func (*Worker) Stop() string { return "stop" }

func Invoke() string { return Use(&Worker{}) + Worker{}.Base() }
`,
			},
		},
		{
			name: "embedded interface",
			updates: map[string]string{
				contractFile: `package sequence

type Base interface {
	Base() string
}

type Runner interface {
	Base
	Run(int) string
	Stop() string
}

type Extended interface {
	Runner
}

func Identity[T ~int](value T) T { return value }

func Use(runner Runner) string {
	return runner.Run(Identity(1)) + runner.Base() + runner.Stop()
}

func UseExtended(runner Extended) string { return Use(runner) }
`,
				implementationFile: `package sequence

type Worker struct{}

func (Worker) Base() string { return "base" }
func (*Worker) Run(int) string { return "run" }
func (*Worker) Stop() string { return "stop" }

func Invoke() string { return UseExtended(&Worker{}) + Worker{}.Base() }
`,
			},
		},
		{
			name: "generic signature",
			updates: map[string]string{
				contractFile: `package sequence

type Base interface {
	Base() string
}

type Runner interface {
	Base
	Run(int) string
	Stop() string
}

type Extended interface {
	Runner
}

func Identity[T ~int | ~int64](value T) T { return value }

func Use(runner Runner) string {
	return runner.Run(Identity(1)) + runner.Base() + runner.Stop()
}

func UseExtended(runner Extended) string { return Use(runner) }
`,
			},
		},
		{
			name: "internal test",
			updates: map[string]string{
				internalTestFile: `package sequence

import "testing"

func TestInvoke(t *testing.T) {
	worker := &Worker{}
	if Invoke()+Use(worker) == "" {
		t.Fatal("empty result")
	}
}
`,
			},
		},
		{
			name: "external test",
			updates: map[string]string{
				externalTestFile: `package sequence_test

import (
	"testing"

	"example.com/sequence"
)

func TestPublicAPI(t *testing.T) {
	worker := &sequence.Worker{}
	if sequence.Invoke()+sequence.Use(worker) == "" {
		t.Fatal("empty result")
	}
}
`,
			},
		},
	}

	scope := ModuleAnalysisScope(moduleDir)
	engine := NewEngine()
	if _, err := engine.Analyze(context.Background(), scope, ChangeSet{}, nil); err != nil {
		t.Fatalf("prime Engine.Analyze() error = %v", err)
	}
	overlays := make(map[string][]byte)
	paths := []string{contractFile, implementationFile, internalTestFile, externalTestFile}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			changes := make([]Change, 0, len(step.updates))
			for path, source := range step.updates {
				overlays[path] = []byte(source)
				changes = append(changes, Change{Path: path, Kind: ChangeContent})
			}
			got, err := engine.Analyze(context.Background(), scope, NewChangeSet(changes...), overlays)
			if err != nil {
				t.Fatalf("Engine.Analyze() error = %v", err)
			}
			want, err := AnalyzeScope(context.Background(), scope, overlays)
			if err != nil {
				t.Fatalf("AnalyzeScope() error = %v", err)
			}
			assertSnapshotsEqual(t, got, want, paths)
		})
	}
}

func TestEngineMatchesFullAnalysisAcrossGoWorkEdits(t *testing.T) {
	workspaceDir := t.TempDir()
	contractDir := filepath.Join(workspaceDir, "contract")
	appDir := filepath.Join(workspaceDir, "app")
	contractFile := filepath.Join(contractDir, "contract.go")
	appFile := filepath.Join(appDir, "app.go")
	workFile := filepath.Join(workspaceDir, "go.work")

	writeAnalysisTestFile(t, filepath.Join(contractDir, "go.mod"), "module example.com/contract\n\ngo 1.26\n")
	writeAnalysisTestFile(t, contractFile, `package contract

type Runner interface { Run() string }
type Worker struct{}
func (Worker) Run() string { return "run" }
func Use(runner Runner) string { return runner.Run() }
`)
	writeAnalysisTestFile(t, filepath.Join(appDir, "go.mod"), "module example.com/app\n\ngo 1.26\n\nrequire example.com/contract v0.0.0\n")
	writeAnalysisTestFile(t, appFile, `package app

import "example.com/contract"

func Invoke() string { return contract.Use(contract.Worker{}) }
`)
	writeAnalysisTestFile(t, workFile, "go 1.26\n\nuse (\n\t./app\n\t./contract\n)\n")

	scope, err := FindAnalysisScope(contractFile, workspaceDir)
	if err != nil {
		t.Fatalf("FindAnalysisScope() error = %v", err)
	}
	engine := NewEngine()
	got, err := engine.Analyze(context.Background(), scope, ChangeSet{}, nil)
	if err != nil {
		t.Fatalf("prime Engine.Analyze() error = %v", err)
	}
	want, err := AnalyzeScope(context.Background(), scope, nil)
	if err != nil {
		t.Fatalf("prime AnalyzeScope() error = %v", err)
	}
	assertSnapshotsEqual(t, got, want, []string{contractFile, appFile})

	workSources := []string{
		"go 1.26\n\nuse ./contract\n",
		"go 1.26\n\nuse (\n\t./app\n\t./contract\n)\n",
	}
	for index, source := range workSources {
		writeAnalysisTestFile(t, workFile, source)
		scope, err = FindAnalysisScope(contractFile, workspaceDir)
		if err != nil {
			t.Fatalf("edit %d FindAnalysisScope() error = %v", index, err)
		}
		got, err = engine.Analyze(
			context.Background(),
			scope,
			NewChangeSet(Change{Path: workFile, Kind: ChangeMetadata}),
			nil,
		)
		if err != nil {
			t.Fatalf("edit %d Engine.Analyze() error = %v", index, err)
		}
		want, err = AnalyzeScope(context.Background(), scope, nil)
		if err != nil {
			t.Fatalf("edit %d AnalyzeScope() error = %v", index, err)
		}
		assertSnapshotsEqual(t, got, want, []string{contractFile, appFile})
	}
}
