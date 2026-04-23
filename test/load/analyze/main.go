// Analyzer: reads the NDJSON ledger, joins k6 and echo records by trace-id,
// and reports drops, misroutes, duplicates, and convergence latency around
// chaos events.
//
// Usage:
//   go run ./test/load/analyze < ledger.ndjson
//   go run ./test/load/analyze -f ledger.ndjson
//   curl http://collector:8080/download | go run ./test/load/analyze
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"github.com/varnish/gateway/test/load/ledger"
)

type entry struct {
	k6   *ledger.Record
	echo []*ledger.Record
}

type result struct {
	total       int
	missingK6   int
	drops       int // k6 has record, no echo record
	misroutes   int // k6.exp != echo.service OR k6.service (header) != exp
	duplicates  int // >1 echo record for a trace-id
	clientErr   int
	non2xx      int
	statusCodes map[int]int
	// convergence: per chaos event, ms to first *correct* response after the
	// event timestamp.
	convergence []convergenceSample
}

type convergenceSample struct {
	event  string
	target string
	ts     int64
	// ms until first correct response after the event. -1 if none observed.
	firstCorrectMs int64
}

func main() {
	var (
		path    string
		verbose bool
	)
	flag.StringVar(&path, "f", "", "Path to NDJSON ledger (default: stdin)")
	flag.BoolVar(&verbose, "v", false, "Print per-misroute detail")
	flag.Parse()

	var in io.Reader = os.Stdin
	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "open %s: %v\n", path, err)
			os.Exit(1)
		}
		defer f.Close()
		in = f
	}

	entries, events, parseErrs := parse(in)
	if parseErrs > 0 {
		fmt.Fprintf(os.Stderr, "warning: %d unparseable lines\n", parseErrs)
	}

	r := analyze(entries, events, verbose)
	printReport(r)

	// Exit non-zero if we found correctness violations, so CI can gate on it.
	if r.drops > 0 || r.misroutes > 0 || r.duplicates > 0 {
		os.Exit(2)
	}
}

func parse(in io.Reader) (map[string]*entry, []ledger.Record, int) {
	entries := map[string]*entry{}
	var events []ledger.Record
	s := bufio.NewScanner(in)
	s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	errs := 0
	for s.Scan() {
		line := s.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec ledger.Record
		if err := json.Unmarshal(line, &rec); err != nil {
			errs++
			continue
		}
		switch rec.Source {
		case ledger.SourceK6:
			e := entries[rec.TraceID]
			if e == nil {
				e = &entry{}
				entries[rec.TraceID] = e
			}
			cp := rec
			e.k6 = &cp
		case ledger.SourceEcho:
			e := entries[rec.TraceID]
			if e == nil {
				e = &entry{}
				entries[rec.TraceID] = e
			}
			cp := rec
			e.echo = append(e.echo, &cp)
		case ledger.SourceChaos:
			events = append(events, rec)
		}
	}
	if err := s.Err(); err != nil {
		fmt.Fprintf(os.Stderr, "scan: %v\n", err)
	}
	return entries, events, errs
}

func analyze(entries map[string]*entry, events []ledger.Record, verbose bool) result {
	r := result{statusCodes: map[int]int{}}

	// Flatten k6 entries sorted by timestamp for convergence scanning.
	type sample struct {
		ts      int64
		correct bool
	}
	var samples []sample

	for tid, e := range entries {
		if e.k6 == nil {
			r.missingK6++
			continue
		}
		r.total++
		r.statusCodes[e.k6.Status]++
		if e.k6.Status == 0 {
			r.clientErr++
		} else if e.k6.Status < 200 || e.k6.Status >= 300 {
			r.non2xx++
		}

		correct := true

		// Header-based misroute: cheap, survives echo POST loss.
		if e.k6.ExpService != "" && e.k6.Service != "" && e.k6.Service != e.k6.ExpService {
			r.misroutes++
			correct = false
			if verbose {
				fmt.Fprintf(os.Stderr, "misroute (header) trace=%s exp=%s got=%s host=%s path=%s\n",
					tid, e.k6.ExpService, e.k6.Service, e.k6.ReqHost, e.k6.ReqPath)
			}
		}

		switch len(e.echo) {
		case 0:
			// Drop only counts for successful client responses — a 5xx
			// with no echo hit is an error, not a drop.
			if e.k6.Status >= 200 && e.k6.Status < 300 {
				r.drops++
				correct = false
			}
		case 1:
			if e.k6.ExpService != "" && e.echo[0].Service != e.k6.ExpService {
				// Only count once: if the header already flagged it, skip.
				if e.k6.Service == "" || e.k6.Service == e.k6.ExpService {
					r.misroutes++
					correct = false
					if verbose {
						fmt.Fprintf(os.Stderr, "misroute (echo) trace=%s exp=%s got=%s pod=%s\n",
							tid, e.k6.ExpService, e.echo[0].Service, e.echo[0].Pod)
					}
				}
			}
		default:
			r.duplicates++
			correct = false
			if verbose {
				pods := make([]string, 0, len(e.echo))
				for _, ec := range e.echo {
					pods = append(pods, ec.Pod)
				}
				fmt.Fprintf(os.Stderr, "duplicate trace=%s count=%d pods=%v\n", tid, len(e.echo), pods)
			}
		}

		samples = append(samples, sample{ts: e.k6.TS, correct: correct})
	}

	sort.Slice(samples, func(i, j int) bool { return samples[i].ts < samples[j].ts })
	for _, ev := range events {
		if ev.Event == "" || ev.Event == "run_start" || ev.Event == "run_end" {
			continue
		}
		cs := convergenceSample{event: ev.Event, target: ev.Target, ts: ev.TS, firstCorrectMs: -1}
		idx := sort.Search(len(samples), func(i int) bool { return samples[i].ts >= ev.TS })
		for i := idx; i < len(samples); i++ {
			if samples[i].correct {
				cs.firstCorrectMs = samples[i].ts - ev.TS
				break
			}
		}
		r.convergence = append(r.convergence, cs)
	}

	return r
}

func printReport(r result) {
	fmt.Printf("=== ledger analysis ===\n")
	fmt.Printf("total requests:       %d\n", r.total)
	fmt.Printf("missing k6 record:    %d (echo-only trace-ids; usually indicates flush loss)\n", r.missingK6)
	fmt.Printf("client errors (st=0): %d\n", r.clientErr)
	fmt.Printf("non-2xx responses:    %d\n", r.non2xx)
	fmt.Printf("drops:                %d\n", r.drops)
	fmt.Printf("misroutes:            %d\n", r.misroutes)
	fmt.Printf("duplicates:           %d\n", r.duplicates)
	if len(r.statusCodes) > 0 {
		codes := make([]int, 0, len(r.statusCodes))
		for c := range r.statusCodes {
			codes = append(codes, c)
		}
		sort.Ints(codes)
		fmt.Printf("status codes:         ")
		for _, c := range codes {
			fmt.Printf("%d=%d ", c, r.statusCodes[c])
		}
		fmt.Println()
	}
	if len(r.convergence) > 0 {
		fmt.Printf("\n=== convergence (first correct response after each event) ===\n")
		for _, c := range r.convergence {
			if c.firstCorrectMs < 0 {
				fmt.Printf("%s %s @ %s: no correct response observed\n",
					c.event, c.target, time.UnixMilli(c.ts).UTC().Format(time.RFC3339))
			} else {
				fmt.Printf("%s %s @ %s: %d ms\n",
					c.event, c.target, time.UnixMilli(c.ts).UTC().Format(time.RFC3339), c.firstCorrectMs)
			}
		}
	}
}
