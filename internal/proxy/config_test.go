package proxy

import "testing"

func TestBuildConfig(t *testing.T) {
	tests := []struct {
		name  string
		udids []string
		want  string
	}{
		{
			name:  "no devices uses plain wildcard range",
			udids: nil,
			want:  "null:9221,:9224-9229",
		},
		{
			name:  "first device pinned to the default device port",
			udids: []string{"0123456789abcdef0123456789abcdef01234567"},
			want:  "null:9221,0123456789abcdef0123456789abcdef01234567:9224,:9225-9229",
		},
		{
			name:  "only the first device is pinned",
			udids: []string{"aaaa", "bbbb"},
			want:  "null:9221,aaaa:9224,:9225-9229",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := BuildConfig(tt.udids); got != tt.want {
				t.Errorf("BuildConfig(%q) = %q, want %q", tt.udids, got, tt.want)
			}
		})
	}
}
