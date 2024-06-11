package main

import (
	"fmt"
	"os"
	"runtime/debug"
	"sync"
)

func Hrs(size int64) string {
	if size < 1024 {
		return fmt.Sprintf("%dB", size)
	}

	units := []string{"KB", "MB", "GB", "TB", "PB", "EB"}
	unitIndex := 0
	sizeInUnits := float64(size) / 1024.0

	for sizeInUnits >= 1024 && unitIndex < len(units)-1 {
		sizeInUnits /= 1024.0
		unitIndex++
	}

	return fmt.Sprintf("%.1f%s", sizeInUnits, units[unitIndex])
}

type ProgressReader struct {
	fp   *os.File
	size int64
	read int64
	mux  sync.Mutex
	cb   func(read int64, size int64)
}

func NewProgressReader(file *os.File, size int64, cb func(read int64, size int64)) *ProgressReader {
	return &ProgressReader{
		fp:   file,
		size: size,
		cb:   cb,
	}
}

func (r *ProgressReader) Read(p []byte) (int, error) {
	n, err := r.fp.Read(p)
	r.mux.Lock()
	r.read += int64(n)
	r.cb(r.read, r.size)
	r.mux.Unlock()
	return n, err
}

func (r *ProgressReader) ReadAt(p []byte, off int64) (int, error) {
	n, err := r.fp.ReadAt(p, off)
	r.mux.Lock()
	r.read = off + int64(n)
	r.cb(r.read, r.size)
	r.mux.Unlock()
	return n, err
}

func (r *ProgressReader) Seek(offset int64, whence int) (int64, error) {
	return r.fp.Seek(offset, whence)
}

func Version() string {
	var revision string
	var modified bool

	bi, ok := debug.ReadBuildInfo()
	if ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				revision = s.Value
			case "vcs.modified":
				if s.Value == "true" {
					modified = true
				}
			}
		}
	}

	if revision == "" {
		return "unavailable"
	}

	if modified {
		return fmt.Sprintf("%s-dirty", revision)
	}

	return revision
}
