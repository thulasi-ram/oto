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

func versionString() string {
	commit := Commit
	if commit == "" {
		if bi, ok := debug.ReadBuildInfo(); ok {
			for _, s := range bi.Settings {
				if s.Key == "vcs.revision" {
					commit = s.Value
				}
			}
		}
	}
	if commit == "" {
		commit = "unknown"
	}
	return fmt.Sprintf("oto %s (commit %s, built %s, %s)", Version, commit, orUnknown(BuildDate), runtime.Version())
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
