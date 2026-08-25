package jobs

// The HF cache, inspected and pruned (ADR-081). Everything here rides typed
// one-shot containers on the server (the probePostgresUID precedent,
// ADR-052): busybox mounting the shared volume, `du` for the listing and a
// PURE-ARGV `rm -rf` for the deletion — no shell ever interpolates an
// operator-supplied string, and the model reference is validated against the
// Hub's own naming rules before it is even mapped to a path.

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"

	"github.com/deepteams/akerdock/internal/dockerruntime"
	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

// hfCacheToolImage is the throwaway the cache one-shots run — pinned, tiny,
// and carrying exactly the two tools needed (du, rm).
const hfCacheToolImage = "busybox:1.37"

// hfCacheMount is where the one-shots see the volume.
const hfCacheMount = "/cache"

// hubRepoRef accepts what the Hub itself accepts for `org/name`: one slash,
// the [A-Za-z0-9._-] charset on both sides. Everything a path traversal or a
// shell would need is outside the charset — the validation is the security
// boundary, the argv-only exec is the belt on top.
var hubRepoRef = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)

// HFCacheEntry is one cached model and its weight on disk.
type HFCacheEntry struct {
	ModelID string
	SizeMB  int
}

// hubDirFor maps `org/name` onto the hub layout's directory name. The Hub
// forbids consecutive dashes in repo names, which is what makes the `--`
// separator unambiguous in both directions.
func hubDirFor(modelID string) (string, error) {
	if !hubRepoRef.MatchString(modelID) || strings.Contains(modelID, "..") || strings.Contains(modelID, "--") {
		return "", fmt.Errorf("%q is not a Hugging Face model reference (org/name)", modelID)
	}
	return "models--" + strings.ReplaceAll(modelID, "/", "--"), nil
}

// hubIDFor is the inverse mapping, for the listing.
func hubIDFor(dir string) (string, bool) {
	rest, ok := strings.CutPrefix(dir, "models--")
	if !ok {
		return "", false
	}
	org, name, ok := strings.Cut(rest, "--")
	if !ok || org == "" || name == "" {
		return "", false
	}
	return org + "/" + name, true
}

func hfCacheHost() *container.HostConfig {
	return &container.HostConfig{Binds: []string{HFCacheVolume + ":" + hfCacheMount}}
}

// ensureCacheTool pulls the one-shot image when the server has never seen it
// — ContainerCreate does not pull, and a virgin GPU server has had no reason
// to hold busybox yet. Inspect-first keeps the steady state to one local
// call; the pull reader is drained so the operation completes.
func ensureCacheTool(ctx context.Context, rt dockerruntime.Runtime) error {
	if _, err := rt.ImageInspect(ctx, hfCacheToolImage); err == nil {
		return nil
	}
	rc, err := rt.ImagePull(ctx, hfCacheToolImage, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("cannot pull %s on the server: %s", hfCacheToolImage, firstLine(err.Error()))
	}
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()
	return nil
}

// HFCacheList reads the cache contents through a one-shot: one `du -sk` per
// hub entry, parsed back into models and MiB, largest first.
func HFCacheList(ctx context.Context, rt dockerruntime.Runtime) ([]HFCacheEntry, error) {
	if err := ensureCacheTool(ctx, rt); err != nil {
		return nil, err
	}
	out, err := runOneShotCapture(ctx, rt, &container.Config{
		Image:      hfCacheToolImage,
		Entrypoint: []string{"sh", "-c"},
		Cmd:        []string{"du -sk " + hfCacheMount + "/hub/models--* 2>/dev/null || true"},
		Labels:     map[string]string{"akerdock.managed": "true"},
	}, hfCacheHost())
	if err != nil {
		return nil, err
	}
	var entries []HFCacheEntry
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		sizeRaw, path, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			sizeRaw, path, ok = strings.Cut(strings.TrimSpace(line), " ")
		}
		if !ok {
			continue
		}
		id, ok := hubIDFor(strings.TrimPrefix(strings.TrimSpace(path), hfCacheMount+"/hub/"))
		if !ok {
			continue
		}
		kb, err := strconv.Atoi(strings.TrimSpace(sizeRaw))
		if err != nil {
			continue
		}
		entries = append(entries, HFCacheEntry{ModelID: id, SizeMB: kb / 1024})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].SizeMB > entries[j].SizeMB })
	return entries, nil
}

// HFCacheDelete removes ONE model's weights. The path is built from the
// validated reference and handed to `rm -rf` as argv — there is no shell in
// this container invocation at all.
func HFCacheDelete(ctx context.Context, rt dockerruntime.Runtime, modelID string) error {
	dir, err := hubDirFor(modelID)
	if err != nil {
		return err
	}
	if err := ensureCacheTool(ctx, rt); err != nil {
		return err
	}
	return runOneShot(ctx, rt, &container.Config{
		Image:      hfCacheToolImage,
		Entrypoint: []string{"rm", "-rf"},
		Cmd:        []string{hfCacheMount + "/hub/" + dir},
		Labels:     map[string]string{"akerdock.managed": "true"},
	}, hfCacheHost())
}

// HFCachePurge empties the whole cache — every top-level entry of the
// volume, dotfiles included, so the reclaim is total.
func HFCachePurge(ctx context.Context, rt dockerruntime.Runtime) error {
	if err := ensureCacheTool(ctx, rt); err != nil {
		return err
	}
	return runOneShot(ctx, rt, &container.Config{
		Image:      hfCacheToolImage,
		Entrypoint: []string{"find", hfCacheMount, "-mindepth", "1", "-maxdepth", "1", "-exec", "rm", "-rf", "{}", "+"},
		Labels:     map[string]string{"akerdock.managed": "true"},
	}, hfCacheHost())
}

// ModelHFToken resolves the token a model container receives: the server's
// own enveloped token when one is stored (ADR-081 — it follows the server),
// the instance-wide fallback otherwise. A token that fails to decrypt is
// surfaced, never silently downgraded to the fallback: the operator stored
// it for a reason.
func ModelHFToken(keyring *envelope.Keyring, server store.Server, fallback string) (string, error) {
	if len(server.HfTokenEnc) == 0 {
		return fallback, nil
	}
	plain, err := keyring.Decrypt("servers", "hf_token_enc", pguuid.String(server.Uuid), server.HfTokenEnc)
	if err != nil {
		return "", fmt.Errorf("the server's HF token does not decrypt: %w", err)
	}
	return string(plain), nil
}
