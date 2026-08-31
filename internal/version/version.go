package version

import (
	"fmt"
	"runtime/debug"

	gethversion "github.com/ethereum/go-ethereum/version"
)

const (
	// ToolName is the command-line program name.
	ToolName = "l2state"
	// GethCommit is the exact go-ethereum source commit for GethVersion.
	GethCommit = "9621c6ad10934a01b5514886fb6fbd87640b6c05"

	gethModulePath         = "github.com/ethereum/go-ethereum"
	developmentToolVersion = "(devel)"
)

var (
	// ToolVersion is the l2state module version recorded by the Go toolchain.
	ToolVersion string
	// GethVersion is the go-ethereum module version linked into the binary.
	GethVersion string

	// buildToolVersion may be injected with -ldflags=-X when the build context
	// deliberately excludes VCS metadata, as production containers do.
	buildToolVersion    string
	compiledGethVersion = gethVersionFromSource()
)

func init() {
	ToolVersion, GethVersion = versions(debug.ReadBuildInfo())
	ToolVersion = overrideToolVersion(ToolVersion, buildToolVersion)
}

func overrideToolVersion(detected, override string) string {
	if override != "" {
		return override
	}
	return detected
}

func versions(info *debug.BuildInfo, ok bool) (string, string) {
	if !ok || info == nil {
		return developmentToolVersion, compiledGethVersion
	}
	toolVersion := info.Main.Version
	if toolVersion == "" {
		toolVersion = developmentToolVersion
	}
	for _, dependency := range info.Deps {
		if dependency == nil || dependency.Path != gethModulePath {
			continue
		}
		for dependency.Replace != nil {
			dependency = dependency.Replace
		}
		if dependency.Version == "" {
			return toolVersion, compiledGethVersion
		}
		return toolVersion, dependency.Version
	}
	return toolVersion, compiledGethVersion
}

func gethVersionFromSource() string {
	version := fmt.Sprintf("v%d.%d.%d", gethversion.Major, gethversion.Minor, gethversion.Patch)
	if gethversion.Meta != "" && gethversion.Meta != "stable" {
		version += "-" + gethversion.Meta
	}
	return version
}
