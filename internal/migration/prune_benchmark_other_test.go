//go:build !darwin && !linux

package migration

func pruneBenchmarkIO() (int64, int64) { return -1, -1 }
