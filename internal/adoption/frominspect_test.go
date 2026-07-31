package adoption

import (
	"reflect"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/go-connections/nat"
)

// TestFromInspectMatchesParseInspects proves the typed conversion and the
// JSON parser read the same facts: the SDK response below and its wire-shape
// JSON must land on the identical Inspect.
func TestFromInspectMatchesParseInspects(t *testing.T) {
	resp := container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{
			ID:   "0123456789abcdefdeadbeef",
			Name: "/legacy-app",
			State: &container.State{
				Status: "running",
			},
			HostConfig: &container.HostConfig{
				Privileged: true,
				CapAdd:     []string{"NET_ADMIN"},
				Resources: container.Resources{
					Devices: []container.DeviceMapping{{PathOnHost: "/dev/net/tun"}},
				},
				PortBindings: nat.PortMap{
					"80/tcp": []nat.PortBinding{{HostPort: "8080"}},
				},
			},
		},
		Config: &container.Config{
			Image:  "nginx:1.27",
			Env:    []string{"A=1"},
			Labels: map[string]string{"com.docker.compose.project": "legacy"},
			ExposedPorts: nat.PortSet{
				"80/tcp": struct{}{},
			},
		},
		Mounts: []container.MountPoint{
			{Type: "volume", Name: "data", Destination: "/data"},
			{Type: "bind", Source: "/srv/conf", Destination: "/etc/conf"},
		},
		NetworkSettings: &container.NetworkSettings{
			Networks: map[string]*network.EndpointSettings{
				"bridge": {IPAddress: "172.17.0.2"},
			},
		},
	}
	if resp.HostConfig != nil {
		resp.HostConfig.NetworkMode = "container:other"
	}

	fromSDK := FromInspect(resp)

	parsed, err := ParseInspects(`[{
		"Id": "0123456789abcdefdeadbeef",
		"Name": "/legacy-app",
		"State": {"Status": "running"},
		"Config": {
			"Image": "nginx:1.27",
			"Env": ["A=1"],
			"Labels": {"com.docker.compose.project": "legacy"},
			"ExposedPorts": {"80/tcp": {}}
		},
		"HostConfig": {
			"NetworkMode": "container:other",
			"Privileged": true,
			"CapAdd": ["NET_ADMIN"],
			"Devices": [{"PathOnHost": "/dev/net/tun"}],
			"PortBindings": {"80/tcp": [{"HostPort": "8080"}]}
		},
		"Mounts": [
			{"Type": "volume", "Name": "data", "Destination": "/data"},
			{"Type": "bind", "Source": "/srv/conf", "Destination": "/etc/conf"}
		],
		"NetworkSettings": {"Networks": {"bridge": {"IPAddress": "172.17.0.2"}}}
	}]`)
	if err != nil || len(parsed) != 1 {
		t.Fatalf("ParseInspects: %v (%d)", err, len(parsed))
	}
	if !reflect.DeepEqual(fromSDK, parsed[0]) {
		t.Fatalf("FromInspect diverges from the wire shape:\n sdk: %+v\njson: %+v", fromSDK, parsed[0])
	}
}

func TestFromInspectToleratesNilSections(t *testing.T) {
	out := FromInspect(container.InspectResponse{})
	if out.ID != "" || out.Config.Image != "" || out.HostConfig.Privileged {
		t.Fatalf("zero response = %+v", out)
	}
	if managed(out, nil) {
		t.Fatal("an empty inspect must not read as managed")
	}
}
