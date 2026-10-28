package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxWindowSamples = 200_000
	maxTotalSamples  = 2_000_000
)

type latRec struct {
	mu     sync.Mutex
	window []time.Duration
	all    []time.Duration
}

func (l *latRec) add(d time.Duration) {
	l.mu.Lock()
	if len(l.window) < maxWindowSamples {
		l.window = append(l.window, d)
	}
	if len(l.all) < maxTotalSamples {
		l.all = append(l.all, d)
	}
	l.mu.Unlock()
}

func (l *latRec) takeWindow() []time.Duration {
	l.mu.Lock()
	w := l.window
	l.window = nil
	l.mu.Unlock()
	sort.Slice(w, func(i, j int) bool { return w[i] < w[j] })
	return w
}

func (l *latRec) total() []time.Duration {
	l.mu.Lock()
	a := append([]time.Duration(nil), l.all...)
	l.mu.Unlock()
	sort.Slice(a, func(i, j int) bool { return a[i] < a[j] })
	return a
}

// pct expects a sorted slice.
func pct(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	return d[int(float64(len(d)-1)*p)]
}

func ms(d time.Duration) string { return strconv.FormatInt(d.Milliseconds(), 10) }

type Stats struct {
	started, ok, failed, aborted, informs, active atomic.Int64
	authChallenges, crReceived, rpcTotal          atomic.Int64
	bytesOut, bytesIn, bytesDownloaded            atomic.Int64
	sessionLat, informLat                         latRec

	mu   sync.Mutex
	rpcs map[string]int64
	errs map[string]int64
}

func newStats() *Stats {
	return &Stats{rpcs: map[string]int64{}, errs: map[string]int64{}}
}

func (s *Stats) countRPC(m string) {
	s.rpcTotal.Add(1)
	s.mu.Lock()
	s.rpcs[m]++
	s.mu.Unlock()
}

func (s *Stats) recordError(err error) {
	msg := err.Error()
	if len(msg) > 160 {
		msg = msg[:160]
	}
	s.mu.Lock()
	s.errs[msg]++
	s.mu.Unlock()
}

func (s *Stats) report(ctx context.Context, done <-chan struct{}, cfg *Config) {
	start := time.Now()
	var w *csv.Writer
	if cfg.CSV != "" {
		f, err := os.Create(cfg.CSV)
		if err != nil {
			log.Fatalf("csv: %v", err)
		}
		defer f.Close()
		w = csv.NewWriter(f)
		w.Write([]string{"time", "elapsed_s", "active", "ok", "failed", "ok_per_s", "fail_per_s", "rpcs",
			"inform_p50_ms", "inform_p95_ms", "inform_p99_ms", "session_p50_ms", "session_p95_ms", "session_p99_ms"})
		defer w.Flush()
	}

	tick := time.NewTicker(cfg.Report)
	defer tick.Stop()
	last := time.Now()
	var lastOK, lastFail, lastRPC int64
	line := func() {
		now := time.Now()
		dt := now.Sub(last).Seconds()
		last = now
		ok, fail, rpc := s.ok.Load(), s.failed.Load(), s.rpcTotal.Load()
		dOK, dFail, dRPC := ok-lastOK, fail-lastFail, rpc-lastRPC
		lastOK, lastFail, lastRPC = ok, fail, rpc
		inf, ses := s.informLat.takeWindow(), s.sessionLat.takeWindow()
		el := now.Sub(start).Truncate(time.Second)
		log.Printf("%8s | active %5d | ok %8d (%7.1f/s) | fail %6d (+%d) | rpcs +%-5d | inform p50/95/99 %s/%s/%s ms | session p50/95/99 %s/%s/%s ms",
			el, s.active.Load(), ok, float64(dOK)/dt, fail, dFail, dRPC,
			ms(pct(inf, .5)), ms(pct(inf, .95)), ms(pct(inf, .99)),
			ms(pct(ses, .5)), ms(pct(ses, .95)), ms(pct(ses, .99)))
		if w != nil {
			w.Write([]string{now.Format(time.RFC3339), fmt.Sprintf("%.0f", el.Seconds()), strconv.FormatInt(s.active.Load(), 10),
				strconv.FormatInt(ok, 10), strconv.FormatInt(fail, 10),
				fmt.Sprintf("%.2f", float64(dOK)/dt), fmt.Sprintf("%.2f", float64(dFail)/dt), strconv.FormatInt(dRPC, 10),
				ms(pct(inf, .5)), ms(pct(inf, .95)), ms(pct(inf, .99)),
				ms(pct(ses, .5)), ms(pct(ses, .95)), ms(pct(ses, .99))})
			w.Flush()
		}
	}

loop:
	for {
		select {
		case <-tick.C:
			line()
		case <-ctx.Done():
			log.Printf("stopping, waiting for in-flight sessions...")
			select {
			case <-done:
			case <-time.After(cfg.Timeout + 2*time.Second):
			}
			break loop
		case <-done:
			break loop
		}
	}
	line()
	s.summary(time.Since(start))
}

func (s *Stats) summary(elapsed time.Duration) {
	fmt.Println()
	fmt.Println("==================== SUMMARY ====================")
	fmt.Printf("elapsed              %s\n", elapsed.Truncate(time.Millisecond))
	fmt.Printf("sessions started     %d\n", s.started.Load())
	fmt.Printf("sessions ok          %d (%.1f/s)\n", s.ok.Load(), float64(s.ok.Load())/elapsed.Seconds())
	fmt.Printf("sessions failed      %d\n", s.failed.Load())
	fmt.Printf("sessions aborted     %d (cut off by shutdown)\n", s.aborted.Load())
	fmt.Printf("informs accepted     %d\n", s.informs.Load())
	fmt.Printf("auth challenges      %d\n", s.authChallenges.Load())
	fmt.Printf("connection requests  %d\n", s.crReceived.Load())
	fmt.Printf("bytes sent/received  %d / %d\n", s.bytesOut.Load(), s.bytesIn.Load())
	if n := s.bytesDownloaded.Load(); n > 0 {
		fmt.Printf("bytes downloaded     %d\n", n)
	}
	for _, x := range []struct {
		name string
		l    *latRec
	}{{"inform RTT", &s.informLat}, {"session time", &s.sessionLat}} {
		a := x.l.total()
		if len(a) == 0 {
			continue
		}
		fmt.Printf("%-20s min %s  p50 %s  p90 %s  p95 %s  p99 %s  max %s ms\n", x.name,
			ms(a[0]), ms(pct(a, .5)), ms(pct(a, .9)), ms(pct(a, .95)), ms(pct(a, .99)), ms(a[len(a)-1]))
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.rpcs) > 0 {
		fmt.Println("\nRPCs received from ACS:")
		for _, kv := range sortedCounts(s.rpcs) {
			fmt.Printf("  %-28s %d\n", kv.k, kv.v)
		}
	}
	if len(s.errs) > 0 {
		fmt.Println("\nTop errors:")
		for i, kv := range sortedCounts(s.errs) {
			if i == 15 {
				break
			}
			fmt.Printf("  %8d  %s\n", kv.v, kv.k)
		}
	}
}

type kv struct {
	k string
	v int64
}

func sortedCounts(m map[string]int64) []kv {
	out := make([]kv, 0, len(m))
	for k, v := range m {
		out = append(out, kv{k, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].v > out[j].v })
	return out
}
