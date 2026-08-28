package main

import (
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/thulasiram/oto/web"
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
	return fmt.Sprintf("oto %s (commit %s, built %s, %s, %s)",
		Version, orUnknown(commitHash()), orUnknown(BuildDate), runtime.Version(), uiState())
}

// uiState reports whether the SPA was built into this binary.
//
// ⭐ IT IS HERE BECAUSE THIS IS THE ONE QUESTION A RELEASE CAN ASK OF A
// DISTROLESS IMAGE. `web/dist` is a build output that is not committed, so the
// only thing between a working image and one that answers `404 page not found`
// at `/` is the Dockerfile's node stage — a step that can be dropped in a
// refactor with every test still green, and that is exactly how the UI came to be
// missing from 0.1.0 through 0.1.2. The runtime image has no shell and `oto api`
// needs a database, so neither `curl` nor a boot is available to a release
// assertion; `docker run <image> version` is. main-image.yml and release.yml
// both fail an image that says `absent`.
//
// ⚠️ THE COUNT IS PART OF THE ANSWER. `embedded (1 file)` is the .gitkeep
// placeholder and nothing else — a boolean would have called that a UI.
func uiState() string {
	if !web.Present() {
		return "ui: absent"
	}
	n := web.Files()
	noun := "files"
	if n == 1 {
		noun = "file"
	}
	return fmt.Sprintf("ui: embedded (%d %s)", n, noun)
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
