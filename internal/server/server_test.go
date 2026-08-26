package server

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nzinovev/agentum/internal/agent"
	"github.com/nzinovev/agentum/internal/config"
	"github.com/nzinovev/agentum/internal/store"
)

// quietLogger returns a discarding logger.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}

// TestNew_MalformedModelsConfigFailsBoot (ADR 0005 D4, the first refusal
// point): a models.yaml with an unknown key is a load error that stops the
// process, naming the file — never a silent fall-back to the defaults.
func TestNew_MalformedModelsConfigFailsBoot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.yaml")
	if err := os.WriteFile(path, []byte("teirs:\n  fast: some-model\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	t.Setenv("AGENTUM_MODELS_CONFIG", path)

	_, err := New(config.Config{}, quietLogger(), &store.Store{})
	if err == nil {
		t.Fatal("a malformed models.yaml must fail boot")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error must name the offending file: %v", err)
	}
}

// TestNew_UnknownAdapterIDFailsBootAndListsKnown (ADR 0005 D1): an unknown
// execution adapter id is a boot error naming the id and listing the known
// ones — the executor is never silently substituted.
func TestNew_UnknownAdapterIDFailsBootAndListsKnown(t *testing.T) {
	_, err := New(config.Config{ExecutionAdapter: "claude-code"}, quietLogger(), &store.Store{})
	if err == nil {
		t.Fatal("an unknown adapter id must fail boot")
	}
	if !strings.Contains(err.Error(), "claude-code") {
		t.Errorf("error must name the unknown id: %v", err)
	}
	if !strings.Contains(err.Error(), string(agent.AdapterOpencode)) {
		t.Errorf("error must list the known ids: %v", err)
	}
}

// TestNew_DefaultBootResolvesTheRegistryDefault (ADR 0005 D1): with no
// configuration at all, boot resolves the registry's default entry — no
// executor is named in config or in the server, and a missing runtime binary
// is NOT a boot failure (D2: unavailability is a probe result).
func TestNew_DefaultBootResolvesTheRegistryDefault(t *testing.T) {
	t.Setenv("AGENTUM_MODELS_CONFIG", filepath.Join(t.TempDir(), "absent.yaml"))
	cfg := config.Config{RuntimeBinary: filepath.Join(t.TempDir(), "no-such-runtime")}
	instance, err := New(cfg, quietLogger(), &store.Store{})
	if err != nil {
		t.Fatalf("boot must succeed with the runtime binary absent: %v", err)
	}
	if got := instance.adapter.Describe().ID; got != agent.AdapterOpencode {
		t.Errorf("default adapter id = %q, want the registry default", got)
	}
}

// recordingHandler collects every log record's message and attributes.
type recordingHandler struct {
	records []string
	attrs   string
}

func (handler *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (handler *recordingHandler) Handle(_ context.Context, record slog.Record) error {
	handler.records = append(handler.records, record.Message)
	record.Attrs(func(attr slog.Attr) bool {
		handler.attrs += attr.Key + "=" + attr.Value.String() + " "
		return true
	})
	return nil
}

func (handler *recordingHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return handler }
func (handler *recordingHandler) WithGroup(name string) slog.Handler       { return handler }

// TestWarmRuntimeProbe_AbsentBinaryLogsNotReady (ADR 0005 D2): the boot-time
// warm-up records the probe outcome. An absent binary logs a not-ready warning
// with the reason; the process keeps starting — the failure surfaces in the
// run that tries to invoke.
func TestWarmRuntimeProbe_AbsentBinaryLogsNotReady(t *testing.T) {
	handler := &recordingHandler{}
	instance := &Server{
		log:     slog.New(handler),
		adapter: agent.NewOpencodeAdapter(filepath.Join(t.TempDir(), "no-such-runtime")),
	}
	instance.warmRuntimeProbe(context.Background())
	if len(handler.records) == 0 {
		t.Fatal("the warm-up must log the probe outcome")
	}
	if !strings.Contains(handler.records[0], "not ready") {
		t.Errorf("log = %v; want the not-ready line", handler.records)
	}
	if !strings.Contains(handler.attrs, "binary not found") {
		t.Errorf("log attrs = %q; want the reason", handler.attrs)
	}
}
