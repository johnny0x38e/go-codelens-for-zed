package analysis

import (
	"testing"
)

func TestDeclarationFingerprintIgnoresBodiesAndTracksAPI(t *testing.T) {
	t.Parallel()

	original := []byte(`package sample

	import "fmt"

	const inferred = 1

	type Box[T any] struct{ Value T }

	func (b Box[T]) Format(prefix string) string {
		return fmt.Sprint(prefix, b.Value)
	}
`)
	bodyEdit := []byte(`package sample

	import "fmt"

	const inferred = 1

	type Box[T any] struct{ Value T }

	func (b Box[T]) Format(prefix string) string {
		// Comments and all statements inside the body are deliberately ignored.
		return prefix
	}
`)
	signatureEdit := []byte(`package sample

	import "fmt"

	const inferred = 1

	type Box[T any] struct{ Value T }

	func (b Box[T]) Format(prefix []byte) string {
		return fmt.Sprint(prefix, b.Value)
	}
`)
	initializerEdit := []byte(`package sample

	import "fmt"

	const inferred = "one"

	type Box[T any] struct{ Value T }

	func (b Box[T]) Format(prefix string) string {
		return fmt.Sprint(prefix, b.Value)
	}
`)

	want, err := declarationFingerprint(original)
	if err != nil {
		t.Fatalf("declarationFingerprint(original) error = %v", err)
	}
	got, err := declarationFingerprint(bodyEdit)
	if err != nil {
		t.Fatalf("declarationFingerprint(body edit) error = %v", err)
	}
	if got != want {
		t.Fatalf("body-only fingerprint = %x, want %x", got, want)
	}
	for name, source := range map[string][]byte{
		"signature":   signatureEdit,
		"initializer": initializerEdit,
	} {
		fingerprint, fingerprintErr := declarationFingerprint(source)
		if fingerprintErr != nil {
			t.Fatalf("declarationFingerprint(%s edit) error = %v", name, fingerprintErr)
		}
		if fingerprint == want {
			t.Errorf("%s edit did not change declaration fingerprint %x", name, want)
		}
	}
}

func TestSourceStructureFingerprintTracksPackageBuildAndImports(t *testing.T) {
	t.Parallel()

	base, err := sourceFingerprintForBytes([]byte(`package sample

import alias "example.com/dependency"
`))
	if err != nil {
		t.Fatalf("sourceFingerprintForBytes(base) error = %v", err)
	}
	aliasOnly, err := sourceFingerprintForBytes([]byte(`package sample

import renamed "example.com/dependency"
`))
	if err != nil {
		t.Fatalf("sourceFingerprintForBytes(alias edit) error = %v", err)
	}
	if aliasOnly.structure != base.structure {
		t.Fatal("import alias edit changed dependency-structure fingerprint")
	}
	for name, source := range map[string][]byte{
		"package": []byte("package renamed\n"),
		"build":   []byte("//go:build darwin\n\npackage sample\n"),
		"import": []byte(`package sample

import "example.com/other"
`),
	} {
		fingerprint, fingerprintErr := sourceFingerprintForBytes(source)
		if fingerprintErr != nil {
			t.Fatalf("sourceFingerprintForBytes(%s edit) error = %v", name, fingerprintErr)
		}
		if fingerprint.structure == base.structure {
			t.Errorf("%s edit did not change structure fingerprint", name)
		}
	}
}
