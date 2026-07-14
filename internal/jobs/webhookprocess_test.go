package jobs

import "testing"

func TestMatchesWatchPaths(t *testing.T) {
	tests := []struct {
		name  string
		files []string
		spec  string
		want  bool
	}{
		{
			name:  "exact file match",
			files: []string{"docs/readme.md"},
			spec:  "docs/readme.md",
			want:  true,
		},
		{
			name:  "directory prefix match",
			files: []string{"src/app/main.go"},
			spec:  "src/",
			want:  true,
		},
		{
			name:  "star suffix is a prefix match",
			files: []string{"web/components/button.ts"},
			spec:  "web/*",
			want:  true,
		},
		{
			name:  "newline-separated patterns",
			files: []string{"api/handler.go"},
			spec:  "docs/\napi/\nweb/",
			want:  true,
		},
		{
			name:  "comma-separated patterns",
			files: []string{"api/handler.go"},
			spec:  "docs/,api/,web/",
			want:  true,
		},
		{
			name:  "space-separated patterns",
			files: []string{"api/handler.go"},
			spec:  "docs/ api/ web/",
			want:  true,
		},
		{
			name:  "no file under any pattern",
			files: []string{"scripts/deploy.sh"},
			spec:  "docs/,web/",
			want:  false,
		},
		{
			name:  "empty spec matches nothing",
			files: []string{"src/main.go"},
			spec:  "",
			want:  false,
		},
		{
			name:  "spec of only separators matches nothing",
			files: []string{"src/main.go"},
			spec:  " ,\n, ",
			want:  false,
		},
		{
			name:  "bare star reduces to an empty pattern and is ignored",
			files: []string{"src/main.go"},
			spec:  "*",
			want:  false,
		},
		{
			name:  "empty file list matches nothing",
			files: nil,
			spec:  "src/",
			want:  false,
		},
		{
			name:  "one matching file among several is enough",
			files: []string{"README.md", "internal/jobs/run.go"},
			spec:  "internal/",
			want:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchesWatchPaths(tt.files, tt.spec); got != tt.want {
				t.Errorf("matchesWatchPaths(%v, %q) = %v, want %v",
					tt.files, tt.spec, got, tt.want)
			}
		})
	}
}
