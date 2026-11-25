// Command fsyncprobe measures the latency of a 4 KiB write followed by a
// durability barrier in a directory (put it on the disk that holds the SQLite
// database). Two barriers are measured:
//
//   - fsync: the fsync(2) system call, which is what SQLite uses for
//     synchronous=FULL by default (PRAGMA fullfsync=0). On Linux it flushes the
//     device write cache; on macOS it does NOT (data may sit in the drive cache).
//
//   - file_sync: Go's (*os.File).Sync, which on macOS issues F_FULLFSYNC (a real
//     cache flush, comparable to Linux fsync durability) and on Linux is fsync.
//
//     fsyncprobe -dir /var/lib/gostack -n 2000
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/pillar/gostack/benchmarks/internal/hist"
)

func main() {
	dir := flag.String("dir", ".", "directory on the database disk")
	n := flag.Int("n", 1000, "iterations per barrier")
	flag.Parse()
	path := filepath.Join(*dir, ".fsyncprobe.tmp")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		log.Fatal(err)
	}
	defer os.Remove(path)
	defer f.Close()
	buf := make([]byte, 4096)
	for i := range buf {
		buf[i] = byte(i)
	}
	out := map[string]any{"os": runtime.GOOS, "arch": runtime.GOARCH, "dir": *dir, "iterations": *n, "write_bytes": len(buf)}
	measure := func(name string, barrier func() error) {
		h := hist.New()
		start := time.Now()
		for i := range *n {
			// Rotate over 256 blocks: overwrite plus occasional extension, like a WAL.
			if _, err := f.WriteAt(buf, int64(i%256)*4096); err != nil {
				log.Fatal(err)
			}
			t := time.Now()
			if err := barrier(); err != nil {
				log.Fatal(err)
			}
			h.Record(time.Since(t))
		}
		s := h.Summary()
		out[name] = map[string]any{"latency": s, "per_second": float64(*n) / time.Since(start).Seconds()}
		log.Printf("%-9s p50=%.3fms p99=%.3fms max=%.3fms", name, s.P50, s.P99, s.Max)
	}
	measure("fsync", func() error { return syscall.Fsync(int(f.Fd())) })
	measure("file_sync", f.Sync)
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
}
