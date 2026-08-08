package agentwire

import (
	"encoding/json"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
)

// TestContainerListFiltersSurviveTheWire pins the property every sweep's
// SAFETY depends on: the label filter must round-trip the channel intact. A
// filter lost in serialization turns "remove this preview's containers" into
// "remove EVERY container, newest first" — which force-removes the agent's
// own helper before anything else.
func TestContainerListFiltersSurviveTheWire(t *testing.T) {
	f := filters.NewArgs(filters.Arg("label", "akerdock.preview_uuid=6d50a89d"))
	p := ContainerListParams{Options: container.ListOptions{All: true, Filters: f}, Filters: EncodeFilters(f)}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("wire: %s", data)
	var back ContainerListParams
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	// The SDK-typed field IS lossy — that is the documented trap this file
	// exists to remember: never rely on Options.Filters after a decode.
	if back.Options.Filters.Len() != 0 {
		t.Log("NOTE: the SDK fixed filters.Args unmarshal — RawFilters can retire")
	}
	decoded, err := back.Filters.Decode()
	if err != nil {
		t.Fatal(err)
	}
	labels := decoded.Get("label")
	if len(labels) != 1 || labels[0] != "akerdock.preview_uuid=6d50a89d" {
		t.Fatalf("filter lost across the wire: %v (payload %s)", labels, data)
	}
}

// TestEncodeFiltersEmptySetEncodesEmpty pins the shortcut: no filters means
// an empty wire string, never the SDK's `{}` JSON for a zero set.
func TestEncodeFiltersEmptySetEncodesEmpty(t *testing.T) {
	if got := EncodeFilters(filters.NewArgs()); got != "" {
		t.Fatalf("empty set encoded as %q", got)
	}
}

// TestPruneFiltersSurviveTheWire pins the same property for the prunes: an
// unfiltered prune would reclaim FOREIGN volumes and networks.
func TestPruneFiltersSurviveTheWire(t *testing.T) {
	f := filters.NewArgs(filters.Arg("label", "akerdock.managed=true"), filters.Arg("dangling", "true"))
	data, err := json.Marshal(PruneParams{Filters: EncodeFilters(f)})
	if err != nil {
		t.Fatal(err)
	}
	var back PruneParams
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	decoded, err := back.Filters.Decode()
	if err != nil {
		t.Fatal(err)
	}
	if got := decoded.Get("label"); len(got) != 1 || got[0] != "akerdock.managed=true" {
		t.Fatalf("label filter lost: %v", got)
	}
	if got := decoded.Get("dangling"); len(got) != 1 || got[0] != "true" {
		t.Fatalf("dangling filter lost: %v", got)
	}
	// And the empty set stays empty, never an error.
	var empty PruneParams
	if err := json.Unmarshal([]byte(`{"filters":""}`), &empty); err != nil {
		t.Fatal(err)
	}
	if decoded, err := empty.Filters.Decode(); err != nil || decoded.Len() != 0 {
		t.Fatalf("empty filters = %v, %v", decoded, err)
	}
}

// TestEveryParamsStructIsWireSafe marshals the zero value of EVERY params
// and result struct of the vocabulary: encoding/json refuses func-typed
// fields outright (image.PullOptions.PrivilegeFunc sank the first real pull
// with "json: unsupported type"), and this sweep is what keeps the next
// SDK-typed field from reaching production before a test does.
func TestEveryParamsStructIsWireSafe(t *testing.T) {
	for _, v := range []any{
		ContainerCreateParams{},
		ContainerStartParams{},
		ContainerStopParams{},
		ContainerRenameParams{},
		ContainerRemoveParams{},
		NameParams{},
		ContainerWaitParams{},
		ContainerListParams{},
		ContainerLogsParams{},
		StatsResult{},
		PruneParams{},
		ContainerExecCreateParams{},
		ContainerExecStartParams{},
		ContainerExecAttachParams{},
		ContainerExecResizeParams{},
		ImagePullParams{},
		ImagePushParams{},
		ImageTagParams{},
		ImageListParams{},
		ImageRemoveParams{},
		VolumeCreateParams{},
		VolumeListParams{},
		VolumeRemoveParams{},
		NetworkCreateParams{},
		NetworkConnectParams{},
		NetworkDisconnectParams{},
		NetworkInspectParams{},
		NetworkListParams{},
		EventsParams{},
		DiskUsageParams{},
		RegistryLoginParams{},
		FileWriteParams{},
		FileReadParams{},
		FileReadResult{},
		FileRemoveParams{},
		FileStatParams{},
		FileStatResult{},
		FileChownParams{},
		FileCopyParams{},
		DirEnsureParams{},
		ExecToFileParams{},
		ExecToFileResult{},
		FileToExecParams{},
		FileToExecResult{},
		FileToURLParams{},
		URLToFileParams{},
		FileHashParams{},
		FileHashResult{},
		ImageBuildParams{},
	} {
		if _, err := json.Marshal(v); err != nil {
			t.Errorf("%T does not survive the wire: %v", v, err)
		}
	}
}
