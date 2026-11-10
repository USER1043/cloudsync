package main

import "runtime/debug"

// buildVersion names this build: the version set at build time if any,
// otherwise the git commit Go recorded when building ("3d3a173", or
// "3d3a173-modified" if there were uncommitted changes), otherwise "dev".
func buildVersion() string {
	if version != "dev" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version
	}
	var rev string
	var modified bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if rev == "" {
		return version
	}
	v := rev[:min(7, len(rev))]
	if modified {
		v += "-modified"
	}
	return v
}
