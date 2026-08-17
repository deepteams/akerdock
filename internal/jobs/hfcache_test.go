package jobs

// The ADR-081 cache surface: the hub-name mapping IS the security boundary
// (everything a traversal or a shell would need is outside its charset), the
// one-shots are asserted on the fake runtime — pinned image, the volume
// bind, and a pure-argv delete with no shell anywhere near operator input.

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/deepteams/akerdock/internal/dockerruntime/fake"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

func TestHubDirMapping(t *testing.T) {
	dir, err := hubDirFor("meta-llama/Llama-3.1-8B")
	if err != nil || dir != "models--meta-llama--Llama-3.1-8B" {
		t.Fatalf("dir = %q, %v", dir, err)
	}
	for _, bad := range []string{
		"noslash", "a/b/c", "../etc/passwd", "org/..", "a b/c", "org/$(rm)",
		"a--b/c", // the Hub forbids consecutive dashes — the separator stays unambiguous
		"/name", "org/", "-x/y",
	} {
		if _, err := hubDirFor(bad); err == nil {
			t.Fatalf("%q must be refused", bad)
		}
	}
	id, ok := hubIDFor("models--org--name")
	if !ok || id != "org/name" {
		t.Fatalf("id = %q %v", id, ok)
	}
	if _, ok := hubIDFor("datasets--org--name"); ok {
		t.Fatal("only model entries are listed")
	}
}

// oneShotRuntime scripts the create→start→wait→logs→remove ladder of a
// one-shot, recording the create for assertions.
func oneShotRuntime(output string) (*fake.Runtime, *container.Config, func() *container.Config) {
	rt := &fake.Runtime{}
	var captured *container.Config
	rt.ContainerCreateFn = func(_ context.Context, cfg *container.Config, host *container.HostConfig, _ *network.NetworkingConfig, _ *ocispec.Platform, _ string) (container.CreateResponse, error) {
		captured = cfg
		if len(host.Binds) != 1 || host.Binds[0] != HFCacheVolume+":"+hfCacheMount {
			return container.CreateResponse{}, context.Canceled
		}
		return container.CreateResponse{ID: "oneshot"}, nil
	}
	rt.ContainerStartFn = func(context.Context, string, container.StartOptions) error { return nil }
	rt.ContainerWaitFn = func(context.Context, string, container.WaitCondition) (<-chan container.WaitResponse, <-chan error) {
		done := make(chan container.WaitResponse, 1)
		done <- container.WaitResponse{StatusCode: 0}
		return done, make(chan error)
	}
	rt.ContainerInspectFn = func(context.Context, string) (container.InspectResponse, error) {
		return container.InspectResponse{ContainerJSONBase: &container.ContainerJSONBase{}}, nil
	}
	rt.ContainerLogsFn = func(context.Context, string, container.LogsOptions) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(output)), nil
	}
	rt.ContainerRemoveFn = func(context.Context, string, container.RemoveOptions) error { return nil }
	return rt, captured, func() *container.Config { return captured }
}

func TestHFCacheListParsesDu(t *testing.T) {
	// The tty=false demux path wraps logs in the stdcopy framing — reuse the
	// scripted runtime whose logs are already demuxed by containerLogsTail
	// via inspect.Config nil (tty=false, raw read fails)… simpler: mark tty.
	rt, _, captured := oneShotRuntime("")
	rt.ContainerInspectFn = func(context.Context, string) (container.InspectResponse, error) {
		return container.InspectResponse{
			ContainerJSONBase: &container.ContainerJSONBase{},
			Config:            &container.Config{Tty: true},
		}, nil
	}
	rt.ContainerLogsFn = func(context.Context, string, container.LogsOptions) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(
			"20480\t" + hfCacheMount + "/hub/models--org--tiny\n" +
				"10485760\t" + hfCacheMount + "/hub/models--meta-llama--Llama-3.1-8B\n" +
				"junk line\n")), nil
	}
	entries, err := HFCacheList(context.Background(), rt)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].ModelID != "meta-llama/Llama-3.1-8B" ||
		entries[0].SizeMB != 10240 || entries[1].SizeMB != 20 {
		t.Fatalf("entries = %+v", entries)
	}
	cfg := captured()
	if cfg.Image != hfCacheToolImage || cfg.Entrypoint[0] != "sh" {
		t.Fatalf("listing one-shot = %+v", cfg)
	}
}

func TestHFCacheDeleteIsPureArgv(t *testing.T) {
	rt, _, captured := oneShotRuntime("")
	if err := HFCacheDelete(context.Background(), rt, "org/name"); err != nil {
		t.Fatal(err)
	}
	cfg := captured()
	// rm -rf as ENTRYPOINT argv, the validated path as the only argument —
	// no shell exists in this container invocation at all.
	if strings.Join(cfg.Entrypoint, " ") != "rm -rf" ||
		len(cfg.Cmd) != 1 || cfg.Cmd[0] != hfCacheMount+"/hub/models--org--name" {
		t.Fatalf("delete one-shot = entrypoint %v cmd %v", cfg.Entrypoint, cfg.Cmd)
	}

	// An invalid reference never reaches the runtime.
	rt2 := &fake.Runtime{}
	if err := HFCacheDelete(context.Background(), rt2, "../escape"); err == nil {
		t.Fatal("traversal must be refused before any container call")
	}
	if calls := rt2.CallNames(); len(calls) != 0 {
		t.Fatalf("the runtime was touched: %v", calls)
	}
}

func TestHFCachePurgeUsesFind(t *testing.T) {
	rt, _, captured := oneShotRuntime("")
	if err := HFCachePurge(context.Background(), rt); err != nil {
		t.Fatal(err)
	}
	cfg := captured()
	if cfg.Entrypoint[0] != "find" || !strings.Contains(strings.Join(cfg.Entrypoint, " "), "-exec rm -rf") {
		t.Fatalf("purge one-shot = %+v", cfg.Entrypoint)
	}
}

// The per-server token wins over the instance fallback, and a token that
// does not decrypt surfaces instead of silently downgrading (ADR-081).
func TestModelHFToken(t *testing.T) {
	_, keyring, _, _ := prevjobsDeps(t)
	server := store.Server{Uuid: pguuid.MustParse(jobFixtureUUID)}

	got, err := ModelHFToken(keyring, server, "hf_instance")
	if err != nil || got != "hf_instance" {
		t.Fatalf("fallback = %q, %v", got, err)
	}

	server.HfTokenEnc = prevjobsEncrypt(t, keyring, "servers", "hf_token_enc", []byte("hf_server"))
	got, err = ModelHFToken(keyring, server, "hf_instance")
	if err != nil || got != "hf_server" {
		t.Fatalf("server token = %q, %v", got, err)
	}

	server.HfTokenEnc = []byte("corrupt")
	if _, err := ModelHFToken(keyring, server, "hf_instance"); err == nil {
		t.Fatal("a corrupt token must surface, never downgrade to the fallback")
	}
}
