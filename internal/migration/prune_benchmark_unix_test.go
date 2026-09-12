//go:build darwin || linux

package migration

import "golang.org/x/sys/unix"

func pruneBenchmarkIO() (int64, int64) {
	var r unix.Rusage
	if unix.Getrusage(unix.RUSAGE_SELF, &r) != nil {
		return -1, -1
	}
	return r.Inblock, r.Oublock
}
