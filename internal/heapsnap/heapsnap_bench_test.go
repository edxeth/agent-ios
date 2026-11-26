package heapsnap

import (
	"fmt"
	"runtime"
	"runtime/metrics"
	"strings"
	"testing"
	"time"
)

// synthSnapshot returns a version 3 Inspector snapshot of about size
// bytes: about a third nodes, two thirds edges, 150 classes, as
// HeapSnapshotBuilder lays it out.
func synthSnapshot(size int) []byte {
	n := size / 52
	var b strings.Builder
	b.Grow(size + size/8)
	b.WriteString(`{"version":3,"type":"Inspector","nodes":[0,0,0,0`)
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, ",%d,%d,%d,%d", i, 16+i%200, 1+i%150, i%4)
	}
	b.WriteString(`],"nodeClassNames":["<root>"`)
	for c := 1; c <= 150; c++ {
		fmt.Fprintf(&b, `,"Class\"%d\""`, c)
	}
	b.WriteString(`],"edges":[`)
	for i := 1; i <= n; i++ {
		if i > 1 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%d,%d,1,%d,%d,%d,2,%d", i-1, i, i%97, i, (i*7)%n+1, i%13)
	}
	b.WriteString(`],"edgeTypes":["Internal","Property","Index","Variable"],"edgeNames":["a","b"]}`)
	return []byte(b.String())
}

// F5 of the frames/heap review: the memory that Parse needs for a 50 MB
// snapshot beyond the snapshot itself (heap diff reads each file once and
// parses it). Run with -bench Parse50MB -benchmem.
func BenchmarkParse50MB(b *testing.B) {
	data := synthSnapshot(50 << 20)
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	var peak uint64
	b.ResetTimer()
	for range b.N {
		b.StopTimer()
		runtime.GC()
		base := heapObjects()
		stop := samplePeak(&peak, base)
		b.StartTimer()
		s, err := Parse(data)
		if err != nil || s.Nodes == 0 {
			b.Fatal(err)
		}
		b.StopTimer()
		stop()
		b.StartTimer()
	}
	b.ReportMetric(float64(len(data))/(1<<20), "snapshot-MiB")
	b.ReportMetric(float64(peak)/(1<<20), "peak-extra-MiB")
}

// heapObjects is the heap memory in objects, live or not yet collected.
func heapObjects() uint64 {
	s := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}
	metrics.Read(s)
	return s[0].Value.Uint64()
}

// samplePeak records in peak the largest heapObjects above base until stop.
func samplePeak(peak *uint64, base uint64) (stop func()) {
	done := make(chan struct{})
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		for {
			if v := heapObjects(); v > base && v-base > *peak {
				*peak = v - base
			}
			select {
			case <-done:
				return
			default:
				time.Sleep(200 * time.Microsecond)
			}
		}
	}()
	return func() { close(done); <-ended }
}
