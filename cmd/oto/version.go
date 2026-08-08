package main

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// Version, Commit and BuildDate are stamped by the linker at build time; see the
// LDFLAGS in the Makefile.
var (
	Version   = "dev"
	Commit    = ""
	BuildDate = ""
)

// commitHash is the linker-stamped commit, falling back to the one the toolchain
// embeds for a `go build` inside a repository.
func commitHash() string {
	if Commit != "" {
		return Commit
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" {
				return s.Value
			}
		}
	}
	return ""
}

func versionString() string {
	return fmt.Sprintf("oto %s (commit %s, built %s, %s)",
		Version, orUnknown(commitHash()), orUnknown(BuildDate), runtime.Version())
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
