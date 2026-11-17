package main

// Development profiling, off unless FORGE_PROFILE=<prefix> is set: the CPU profile runs from start-up
// until GET /__profile/stop, which writes <prefix>.cpu and <prefix>.allocs.

import (
	"os"
	"runtime"
	"runtime/pprof"
	"sync"
)

var profilePrefix string
var profileFile *os.File
var profileOnce sync.Once

func startProfile(prefix string) {
	cpu, err := os.Create(prefix + ".cpu")
	if err != nil {
		return
	}
	runtime.MemProfileRate = 64 * 1024
	if err := pprof.StartCPUProfile(cpu); err != nil {
		return
	}
	profilePrefix, profileFile = prefix, cpu
}

func stopProfile() {
	profileOnce.Do(func() {
		if profileFile == nil {
			return
		}
		pprof.StopCPUProfile()
		_ = profileFile.Close()
		if allocs, err := os.Create(profilePrefix + ".allocs"); err == nil {
			_ = pprof.Lookup("allocs").WriteTo(allocs, 0)
			_ = allocs.Close()
		}
	})
}
