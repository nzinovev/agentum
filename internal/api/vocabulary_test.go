package api

// Vocabulary guards. The launch entity is a run; the old word is reserved
// for the work item, which is not an entity yet. These tests exist so the vocabulary
// cannot drift back one handler at a time: each new route or response field
// named the old way fails here, naming the exact spot, instead of surviving
// until a reviewer greps for it.

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nzinovev/agentum/internal/manifest"
)

// routeRecorder stands in for *http.ServeMux and remembers every pattern
// Register mounts. ServeMux itself offers no way to enumerate registered
// patterns, which is exactly what the route guard needs.
type routeRecorder struct {
	patterns []string
}

func (recorder *routeRecorder) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	recorder.patterns = append(recorder.patterns, pattern)
}

// TestRouteVocabularyHasNoTaskToken fails when any registered path names the
// launch entity by the old word. Segments are compared whole, not as
// substrings: a substring check would either miss real renames back or start
// flagging unrelated words that merely contain the letters.
func TestRouteVocabularyHasNoTaskToken(t *testing.T) {
	t.Parallel()
	recorder := &routeRecorder{}
	New(nil, nil, nil, nil).Register(recorder)
	if len(recorder.patterns) == 0 {
		t.Fatal("no routes registered — did Register change shape?")
	}
	for _, pattern := range recorder.patterns {
		_, path, hasMethod := strings.Cut(pattern, " ")
		if !hasMethod {
			path = pattern
		}
		for _, segment := range strings.Split(path, "/") {
			if segment == "task" || segment == "tasks" {
				t.Errorf("route %q: segment %q names the launch entity; the vocabulary word is \"runs\"", pattern, segment)
			}
		}
	}
}

// TestResponseJSONTagsHaveNoTaskToken walks the public response shapes and
// fails on any json tag carrying the old token. The roots are listed by hand:
// an automatic package walk would drag in internal types whose tags mirror
// physical column names, and the guard would then demand breaking those.
func TestResponseJSONTagsHaveNoTaskToken(t *testing.T) {
	t.Parallel()
	roots := []any{
		projectResponse{},
		runResponse{},
		invocationResponse{},
		artifactRevisionResponse{},
		manifestResponse{},
		sealInfoResponse{},
		finalReviewResponse{},
		publicationResponse{},
		errorBody{},
		manifest.Diff{},
	}
	for _, root := range roots {
		walkJSONTags(t, reflect.TypeOf(root), reflect.TypeOf(root).String())
	}
}

// walkJSONTags descends through pointers, slices, arrays, and maps into every
// struct reachable from rootType and checks each field's json tag name. Tag
// names are split on "_" and compared as tokens, mirroring the route guard's
// whole-segment rule.
func walkJSONTags(t *testing.T, typeOfValue reflect.Type, path string) {
	t.Helper()
	switch typeOfValue.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		walkJSONTags(t, typeOfValue.Elem(), path)
		return
	case reflect.Map:
		walkJSONTags(t, typeOfValue.Key(), path+"[key]")
		walkJSONTags(t, typeOfValue.Elem(), path+"[value]")
		return
	case reflect.Struct:
	default:
		return
	}
	for fieldIndex := 0; fieldIndex < typeOfValue.NumField(); fieldIndex++ {
		field := typeOfValue.Field(fieldIndex)
		if field.PkgPath != "" {
			continue // unexported: never part of the JSON surface
		}
		fieldPath := path + "." + field.Name
		tagName, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		for _, token := range strings.Split(tagName, "_") {
			if token == "task" || token == "tasks" {
				t.Errorf("%s: json tag %q carries the reserved token %q; the vocabulary word is \"run\"", fieldPath, tagName, token)
			}
		}
		walkJSONTags(t, field.Type, fieldPath)
	}
}

// TestEventVocabularyHasNoTaskPrefix fails on any event-type literal in
// internal/ still prefixed with the reserved word. Event types are declared as
// raw strings in two packages with no compiler-visible link between them, so a
// rename that misses one package compiles cleanly — only a text scan catches
// it. The needle is assembled at runtime so this file does not carry it and
// catch itself.
func TestEventVocabularyHasNoTaskPrefix(t *testing.T) {
	t.Parallel()
	// Split so this line's own source carries no verbatim needle — a plain
	// two-literal assembly would match itself, the quote sitting directly
	// before the word in the source text.
	needle := string('"') + "task" + "."
	internalRoot := filepath.Join("..", "..", "internal")
	err := filepath.WalkDir(internalRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			return nil
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for lineNumber, line := range strings.Split(string(content), "\n") {
			if strings.Contains(line, needle) {
				t.Errorf("%s:%d: event-type literal %s… uses the reserved prefix; the vocabulary prefix is run.", path, lineNumber+1, needle)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk internal/: %v", err)
	}
}

// TestDocsHaveNoTasksRoutes fails when docs/*.md mention a /tasks path. The
// direct copy of the acceptance grep: documentation does not compile, so a
// stale path there is invisible to every other guard.
func TestDocsHaveNoTasksRoutes(t *testing.T) {
	t.Parallel()
	docsDir := filepath.Join("..", "..", "docs")
	entries, readErr := os.ReadDir(docsDir)
	if readErr != nil {
		t.Fatalf("read docs/: %v", readErr)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		content, readErr := os.ReadFile(filepath.Join(docsDir, entry.Name()))
		if readErr != nil {
			t.Fatalf("read %s: %v", entry.Name(), readErr)
		}
		for lineNumber, line := range strings.Split(string(content), "\n") {
			if strings.Contains(line, "/tasks") {
				t.Errorf("docs/%s:%d: path uses the reserved \"tasks\" segment; the vocabulary word is \"runs\"", entry.Name(), lineNumber+1)
			}
		}
	}
}
