package jobs

import (
	"reflect"
	"testing"
)

func TestParseRemnants(t *testing.T) {
	tests := []struct {
		name       string
		out        string
		containers []string
		volumes    []string
		files      []string
	}{
		{
			name: "empty output",
			out:  "",
		},
		{
			name: "headers only, no remnants",
			out:  "---containers\n---volumes\n---files\n",
		},
		{
			name: "one entry per section",
			out: "---containers\napp-web-1\n" +
				"---volumes\n0195a0b0_data\n" +
				"---files\n/data/akerdock/applications/0195a0b0\n",
			containers: []string{"app-web-1"},
			volumes:    []string{"0195a0b0_data"},
			files:      []string{"/data/akerdock/applications/0195a0b0"},
		},
		{
			name: "multiple entries and blank lines are skipped",
			out: "---containers\napp-web-1\n\napp-web-2\n" +
				"---volumes\n\n---files\n",
			containers: []string{"app-web-1", "app-web-2"},
		},
		{
			name: "surrounding whitespace is trimmed",
			out:  "---containers\n  app-web-1  \n---volumes\n\t0195_data\n---files\n",
			containers: []string{
				"app-web-1",
			},
			volumes: []string{"0195_data"},
		},
		{
			name: "text before any section header is dropped",
			out:  "noise from the shell\n---containers\napp-web-1\n",
			containers: []string{
				"app-web-1",
			},
		},
		{
			name: "unknown section is ignored",
			out:  "---networks\nbridge\n---containers\napp-web-1\n",
			containers: []string{
				"app-web-1",
			},
		},
		{
			name:    "section change resets the target",
			out:     "---containers\n---volumes\nvol-1\n---containers\nc-1\n",
			volumes: []string{"vol-1"},
			containers: []string{
				"c-1",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			containers, volumes, files := parseRemnants(tt.out)
			if !reflect.DeepEqual(containers, tt.containers) {
				t.Errorf("containers = %#v, want %#v", containers, tt.containers)
			}
			if !reflect.DeepEqual(volumes, tt.volumes) {
				t.Errorf("volumes = %#v, want %#v", volumes, tt.volumes)
			}
			if !reflect.DeepEqual(files, tt.files) {
				t.Errorf("files = %#v, want %#v", files, tt.files)
			}
		})
	}
}
