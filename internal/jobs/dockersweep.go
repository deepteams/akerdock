// Typed replacements for the `docker … ls -q --filter | xargs -r docker …
// rm -f` sweeps (ADR-051): enumerate by filter, remove item by item, tolerate
// objects that vanished underway — and report the first real failure, so a
// partial cleanup is never recorded as clean.
package jobs

import (
	"context"
	"strings"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"

	"github.com/deepteams/akerdock/internal/dockerruntime"
)

// managedResourceFilter selects a resource's managed objects (§2.3): both
// labels must match, so nothing foreign is ever swept.
func managedResourceFilter(resourceUUID string) filters.Args {
	return filters.NewArgs(
		filters.Arg("label", "akerdock.managed=true"),
		filters.Arg("label", "akerdock.resource_uuid="+resourceUUID),
	)
}

// removeNamedContainers force-removes containers by name, tolerating absent
// ones — the typed `docker rm -f a b || true`.
func removeNamedContainers(ctx context.Context, rt dockerruntime.Runtime, removeVolumes bool, names ...string) error {
	for _, name := range names {
		err := rt.ContainerRemove(ctx, name, container.RemoveOptions{Force: true, RemoveVolumes: removeVolumes})
		if err != nil && !dockerruntime.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// sweepContainers force-removes every container matching the filter.
func sweepContainers(ctx context.Context, rt dockerruntime.Runtime, f filters.Args, removeVolumes bool) error {
	list, err := rt.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return err
	}
	for _, c := range list {
		err := rt.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true, RemoveVolumes: removeVolumes})
		if err != nil && !dockerruntime.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// sweepVolumes force-removes every volume matching the filter.
func sweepVolumes(ctx context.Context, rt dockerruntime.Runtime, f filters.Args) error {
	list, err := rt.VolumeList(ctx, volume.ListOptions{Filters: f})
	if err != nil {
		return err
	}
	for _, v := range list.Volumes {
		if v == nil {
			continue
		}
		if err := rt.VolumeRemove(ctx, v.Name, true); err != nil && !dockerruntime.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// sweepNetworks removes every network matching the filter.
func sweepNetworks(ctx context.Context, rt dockerruntime.Runtime, f filters.Args) error {
	list, err := rt.NetworkList(ctx, network.ListOptions{Filters: f})
	if err != nil {
		return err
	}
	for _, n := range list {
		if err := rt.NetworkRemove(ctx, n.ID); err != nil && !dockerruntime.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// sweepImagesByReference force-removes every image of a repository — the
// typed `docker images -q <repo> | xargs -r docker rmi -f`.
func sweepImagesByReference(ctx context.Context, rt dockerruntime.Runtime, repository string) error {
	list, err := rt.ImageList(ctx, image.ListOptions{
		Filters: filters.NewArgs(filters.Arg("reference", repository)),
	})
	if err != nil {
		return err
	}
	for _, img := range list {
		_, err := rt.ImageRemove(ctx, img.ID, image.RemoveOptions{Force: true})
		if err != nil && !dockerruntime.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// containerName is a summary's own name, slash-stripped.
func containerName(c container.Summary) string {
	if len(c.Names) == 0 {
		return ""
	}
	return strings.TrimPrefix(c.Names[0], "/")
}
