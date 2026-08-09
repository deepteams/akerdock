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

// TestWakeResourceVocabularyIsStable pins the two things the control plane's
// half of ADR-067 is written against: the method NAME, which is what an agent
// too old to know it answers `unimplemented` for — the mint's whole
// compatibility signal — and the field names of the wake's request and verdict.
// Renaming either silently turns "cannot wake" into the answer for every
// server, and every session mint against a sleeping resource would refuse.
func TestWakeResourceVocabularyIsStable(t *testing.T) {
	if MethodWakeResource != "WakeResource" {
		t.Fatalf("method renamed to %q — older agents answer unimplemented for the old name", MethodWakeResource)
	}
	data, err := json.Marshal(WakeResourceParams{ResourceUUID: "6d50a89d"})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"resource_uuid":"6d50a89d"}` {
		t.Fatalf("params wire form = %s", data)
	}
	// An empty wake is the already-awake case, and it must not marshal a null:
	// the control plane reads "started nothing" from an absent list.
	res, err := json.Marshal(WakeResourceResult{})
	if err != nil {
		t.Fatal(err)
	}
	if string(res) != `{}` {
		t.Fatalf("empty result wire form = %s", res)
	}
	var back WakeResourceResult
	if err := json.Unmarshal([]byte(`{"started":["db","web"]}`), &back); err != nil {
		t.Fatal(err)
	}
	if len(back.Started) != 2 || back.Started[0] != "db" || back.Started[1] != "web" {
		t.Fatalf("started lost across the wire: %v", back.Started)
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
		IngressExpectParams{},
		IngressCutParams{},
		WakeResourceParams{},
		WakeResourceResult{},
	} {
		if _, err := json.Marshal(v); err != nil {
			t.Errorf("%T does not survive the wire: %v", v, err)
		}
	}
}
