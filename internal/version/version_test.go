package version

import (
	"runtime/debug"
	"testing"
)

func TestVersions(t *testing.T) {
	tests := []struct {
		name     string
		info     *debug.BuildInfo
		ok       bool
		wantTool string
		wantGeth string
	}{
		{
			name:     "build info unavailable",
			wantTool: developmentToolVersion,
			wantGeth: compiledGethVersion,
		},
		{
			name: "module versions",
			info: &debug.BuildInfo{
				Main: debug.Module{Version: "v0.4.0"},
				Deps: []*debug.Module{
					nil,
					{Path: "example.com/other", Version: "v1.0.0"},
					{Path: gethModulePath, Version: "v1.17.5"},
				},
			},
			ok:       true,
			wantTool: "v0.4.0",
			wantGeth: "v1.17.5",
		},
		{
			name: "replacement version",
			info: &debug.BuildInfo{
				Main: debug.Module{Version: "v0.4.0"},
				Deps: []*debug.Module{{
					Path:    gethModulePath,
					Version: "v1.17.5",
					Replace: &debug.Module{Path: "example.com/geth-fork", Version: "v1.17.6-0.20260830000000-abcdef123456"},
				}},
			},
			ok:       true,
			wantTool: "v0.4.0",
			wantGeth: "v1.17.6-0.20260830000000-abcdef123456",
		},
		{
			name: "development build and local replacement",
			info: &debug.BuildInfo{
				Deps: []*debug.Module{{
					Path:    gethModulePath,
					Version: "v1.17.5",
					Replace: &debug.Module{Path: "../go-ethereum"},
				}},
			},
			ok:       true,
			wantTool: developmentToolVersion,
			wantGeth: compiledGethVersion,
		},
		{
			name:     "geth module missing",
			info:     &debug.BuildInfo{Main: debug.Module{Version: "v0.4.0"}},
			ok:       true,
			wantTool: "v0.4.0",
			wantGeth: compiledGethVersion,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotTool, gotGeth := versions(test.info, test.ok)
			if gotTool != test.wantTool || gotGeth != test.wantGeth {
				t.Fatalf("versions() = (%q, %q), want (%q, %q)", gotTool, gotGeth, test.wantTool, test.wantGeth)
			}
		})
	}
}

func TestRuntimeVersions(t *testing.T) {
	if ToolVersion == "" {
		t.Fatal("ToolVersion is empty")
	}
	if GethVersion != "v1.17.5" {
		t.Fatalf("GethVersion = %q, want v1.17.5", GethVersion)
	}
}
