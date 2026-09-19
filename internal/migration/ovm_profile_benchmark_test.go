package migration

import (
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"testing"
)

// Optional diagnostic runs only: select one leaf benchmark with -benchtime=1x.
// Keep profiles separate from uninstrumented paired timing observations.
func profileOVMBenchmark(b *testing.B) func() {
	b.Helper()
	dir := os.Getenv("L2STATE_BENCH_PROFILE_DIR")
	if dir == "" {
		return func() {}
	}
	if b.N != 1 {
		b.Fatal("OVM profiling requires -benchtime=1x")
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		b.Fatal(err)
	}
	write := func(name, profile string) {
		b.Helper()
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			b.Fatal(err)
		}
		err = pprof.Lookup(profile).WriteTo(f, 0)
		closeErr := f.Close()
		if err != nil {
			b.Fatal(err)
		}
		if closeErr != nil {
			b.Fatal(closeErr)
		}
	}
	// GC makes heap accounting current. Subtract alloc-before from alloc-after
	// with pprof -base to remove fixture setup from cumulative allocation samples.
	runtime.GC()
	write("alloc-before.pprof", "allocs")
	f, err := os.Create(filepath.Join(dir, "cpu.pprof"))
	if err != nil {
		b.Fatal(err)
	}
	if err := pprof.StartCPUProfile(f); err != nil {
		_ = f.Close()
		b.Fatal(err)
	}
	runtime.SetBlockProfileRate(1)
	oldMutex := runtime.SetMutexProfileFraction(1)
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		pprof.StopCPUProfile()
		runtime.SetBlockProfileRate(0)
		runtime.SetMutexProfileFraction(oldMutex)
		if err := f.Close(); err != nil {
			b.Fatal(err)
		}
		runtime.GC()
		write("alloc-after.pprof", "allocs")
		write("block.pprof", "block")
		write("mutex.pprof", "mutex")
	}
	b.Cleanup(stop)
	return stop
}
