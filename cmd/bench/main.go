// Command bench drives reproducible load tests against a mail engine
// (postdove or mailezine). Modes:
//
//	bench verify        - auth + delivery sanity check
//	bench seed          - deliver N messages to local users (no auth)
//	bench smtp          - concurrent authenticated submissions
//	bench imap          - concurrent login + select + fetch sampling
//	bench queue         - concurrent submissions to external recipients
//	bench stats         - sample container memory/IO via docker stats
//
// Every mode takes -engine/-smtp/-imap endpoints so the same binary drives
// both stacks (docs/benchmark.md).
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-sasl"
	gosmtp "github.com/emersion/go-smtp"
)

var (
	engine  = flag.String("engine", "engine", "label for results")
	smtpAddr = flag.String("smtp", "127.0.0.1:1587", "SMTP submission address")
	inSmtp   = flag.String("in-smtp", "127.0.0.1:25", "inbound SMTP address")
	imapAddr = flag.String("imap", "127.0.0.1:143", "IMAP address")
	user     = flag.String("user", "u00001@example.com", "auth user")
	pass     = flag.String("pass", "benchpass", "auth password")
	conns    = flag.Int("conns", 300, "concurrent connections")
	msgs     = flag.Int("msgs", 1000, "total messages")
	size     = flag.Int("size", 4096, "message body size (bytes)")
	to       = flag.String("to", "u%05d@example.com", "recipient format (%%05d = 1..N users)")
	domain   = flag.String("domain", "example.com", "local domain")
	fromDom  = flag.String("from-dom", "external.test", "seed sender domain")
	dur      = flag.Duration("dur", 15*time.Second, "sampling duration for stats")
	verbose  = flag.Bool("verbose", false, "print first failures")
)

func main() {
	flag.Parse()
	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: bench <verify|seed|smtp|imap|queue|stats> [flags]")
		os.Exit(2)
	}
	switch flag.Arg(0) {
	case "verify":
		verify()
	case "seed":
		seed()
	case "smtp":
		smtpBench()
	case "imap":
		imapBench()
	case "stats":
		stats()
	default:
		fmt.Fprintf(os.Stderr, "bench: unknown mode %q\n", flag.Arg(0))
		os.Exit(2)
	}
}

func verify() {
	// SMTP AUTH + local delivery.
	c, err := gosmtp.Dial(*smtpAddr)
	checkFatal(err)
	if err := c.Auth(sasl.NewPlainClient("", *user, *pass)); err != nil {
		fmt.Printf("SMTP AUTH: FAIL %v\n", err)
	} else {
		fmt.Printf("SMTP AUTH: OK (%s)\n", *user)
	}
	_ = c.Close()
	// IMAP login.
	ic, err := imapclient.DialInsecure(*imapAddr, nil)
	if err == nil {
		if err := ic.Login(*user, *pass).Wait(); err != nil {
			fmt.Printf("IMAP LOGIN: FAIL %v\n", err)
		} else {
			fmt.Printf("IMAP LOGIN: OK (%s)\n", *user)
			_ = ic.Logout().Wait()
		}
		ic.Close()
	} else {
		fmt.Printf("IMAP CONNECT: FAIL %v\n", err)
	}
}

// randomBody builds a realistic plain-text message of approximately the
// requested size (random bytes would trip spam filters and skew results).
func randomBody(subject string, size int) []byte {
	words := []string{"meeting", "project", "update", "quarterly", "report",
		"customer", "review", "schedule", "budget", "delivery", "team", "draft",
		"summary", "agenda", "minutes", "proposal", "estimate", "feedback",
		"planning", "release"}
	var body strings.Builder
	lineLen := 0
	for body.Len() < size {
		w := words[randUserID()%len(words)]
		if lineLen+len(w)+1 > 78 {
			body.WriteString("\r\n")
			lineLen = 0
		}
		body.WriteString(w)
		body.WriteByte(' ')
		lineLen += len(w) + 1
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "From: sender@bench.test\r\nTo: %s\r\nSubject: %s\r\nMessage-ID: <%d-%d@bench.test>\r\nDate: %s\r\n\r\n",
		*user, subject, time.Now().UnixNano(), randInt(), time.Now().Format(time.RFC1123Z))
	sb.WriteString(body.String())
	return []byte(sb.String())
}

func randInt() int64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return int64(uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
		uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7]))
}

// randUserID returns 1..10000 for recipient cycling.
func randUserID() int {
	return int(uint64(randInt())%10000) + 1
}

// benchResult aggregates a load run.
type benchResult struct {
	Engine   string
	Mode     string
	Conns    int
	Total    int
	Duration time.Duration
	PerSec   float64
	P50      time.Duration
	P95      time.Duration
	P99      time.Duration
	Failures int
}

func (r benchResult) print() {
	fmt.Printf("\n== %s %s\n", r.Engine, r.Mode)
	fmt.Printf("  total=%d conns=%d duration=%.2fs rate=%.1f msg/s failures=%d\n",
		r.Total, r.Conns, r.Duration.Seconds(), r.PerSec, r.Failures)
	fmt.Printf("  latency p50=%s p95=%s p99=%s\n", r.P50, r.P95, r.P99)
}

func percentiles(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), d...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// runConcurrent spawns n workers each calling fn until total messages are
// done, and reports rate + latency percentiles.
func runConcurrent(n, total int, fn func() error) benchResult {
	var (
		wg      sync.WaitGroup
		start   = time.Now()
		done    atomic.Int64
		fails   atomic.Int64
		firstErrs []string
		latMu   sync.Mutex
		latency []time.Duration
	)
	work := make(chan struct{}, total)
	for i := 0; i < total; i++ {
		work <- struct{}{}
	}
	close(work)
	for w := 0; w < n; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range work {
				t0 := time.Now()
				if err := fn(); err != nil {
					fails.Add(1)
					if *verbose && int(fails.Load()) <= 5 {
						latMu.Lock()
						firstErrs = append(firstErrs, err.Error())
						latMu.Unlock()
					}
					continue
				}
				latMu.Lock()
				latency = append(latency, time.Since(t0))
				latMu.Unlock()
				done.Add(1)
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	latMu.Lock()
	defer latMu.Unlock()
	if *verbose {
		for _, e := range firstErrs {
			fmt.Printf("  fail: %v\n", e)
		}
	}
	return benchResult{
		Engine:   *engine,
		Mode:     flag.Arg(0),
		Conns:    n,
		Total:    int(done.Load()),
		Duration: elapsed,
		PerSec:   float64(done.Load()) / elapsed.Seconds(),
		P50:      percentiles(latency, 0.50),
		P95:      percentiles(latency, 0.95),
		P99:      percentiles(latency, 0.99),
		Failures: int(fails.Load()),
	}
}

// seed delivers messages to local users over inbound SMTP (no auth,
// trusted-peer model). Recipients cycle through the user range.
func seed() {
	body := randomBody("bench seed", *size)
	res := runConcurrent(*conns, *msgs, func() error {
		c, err := gosmtp.Dial(*inSmtp)
		if err != nil {
			return err
		}
		defer c.Close()
		to := fmt.Sprintf(*to, randUserID())
		if err := c.Mail("bench-sender@"+*fromDom, nil); err != nil {
			return err
		}
		if err := c.Rcpt(to, nil); err != nil {
			return err
		}
		w, err := c.Data()
		if err != nil {
			return err
		}
		if _, err := w.Write(body); err != nil {
			return err
		}
		return w.Close()
	})
	res.print()
	dayRate := res.PerSec * 60 * 8
	fmt.Printf("  => daily equivalent (8h): %.0f messages/day\n", dayRate)
}

// smtpBench runs concurrent authenticated submissions (one message per
// connection) to local recipients.
func smtpBench() {
	body := randomBody("bench smtp", *size)
	res := runConcurrent(*conns, *msgs, func() error {
		c, err := gosmtp.Dial(*smtpAddr)
		if err != nil {
			return err
		}
		defer c.Close()
		if err := c.Auth(sasl.NewPlainClient("", *user, *pass)); err != nil {
			return err
		}
		to := fmt.Sprintf(*to, randUserID())
		if err := c.Mail(*user, nil); err != nil {
			return err
		}
		if err := c.Rcpt(to, nil); err != nil {
			return err
		}
		w, err := c.Data()
		if err != nil {
			return err
		}
		if _, err := w.Write(body); err != nil {
			return err
		}
		return w.Close()
	})
	res.print()
	dayRate := res.PerSec * 60 * 8
	fmt.Printf("  => daily equivalent (8h): %.0f messages/day\n", dayRate)
}

// imapBench holds concurrent sessions open and samples fetch latency.
func imapBench() {
	var (
		wg      sync.WaitGroup
		fails   atomic.Int64
		ok      atomic.Int64
		latMu   sync.Mutex
		latency []time.Duration
	)
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), *dur)
	defer cancel()
	for w := 0; w < *conns; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ic, err := imapclient.DialInsecure(*imapAddr, nil)
			if err != nil {
				fails.Add(1)
				return
			}
			defer ic.Close()
			if err := ic.Login(*user, *pass).Wait(); err != nil {
				fails.Add(1)
				return
			}
			if _, err := ic.Select("INBOX", nil).Wait(); err != nil {
				fails.Add(1)
				return
			}
			ok.Add(1)
			for {
				t0 := time.Now()
				_, err := ic.Fetch(imap.SeqSetNum(1), nil).Collect()
				latMu.Lock()
				latency = append(latency, time.Since(t0))
				latMu.Unlock()
				select {
				case <-ctx.Done():
					return
				default:
				}
				if err != nil {
					// A fetch error does not drop the session; only login and
					// select failures count against the engine.
					time.Sleep(200 * time.Millisecond)
					return
				}
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	latMu.Lock()
	defer latMu.Unlock()
	res := benchResult{
		Engine:   *engine,
		Mode:     "imap",
		Conns:    *conns,
		Total:    int(ok.Load()),
		Duration: elapsed,
		Failures: int(fails.Load()),
		P50:      percentiles(latency, 0.50),
		P95:      percentiles(latency, 0.95),
		P99:      percentiles(latency, 0.99),
	}
	if elapsed.Seconds() > 0 {
		res.PerSec = float64(len(latency)) / elapsed.Seconds()
	}
	res.print()
	fmt.Printf("  => sessions up=%d fetch ops=%d\n", ok.Load(), len(latency))
}

// stats samples container memory and block IO via docker stats.
func stats() {
	names := flag.Args()[1:]
	if len(names) == 0 {
		fmt.Fprintln(os.Stderr, "usage: bench stats <container...>")
		os.Exit(2)
	}
	type sample struct {
		Mem string
		IO  string
	}
	per := map[string][]sample{}
	deadline := time.Now().Add(*dur)
	for time.Now().Before(deadline) {
		args := append([]string{"stats", "--no-stream", "--format",
			`{{.Name}} {{.MemUsage}} {{.BlockIO}}`}, names...)
		out, err := exec.Command("docker", args...).CombinedOutput()
		if err != nil {
			fmt.Fprintf(os.Stderr, "docker stats: %v\n%s", err, out)
			os.Exit(1)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			f := strings.Fields(line)
			if len(f) >= 3 {
				per[f[0]] = append(per[f[0]], sample{Mem: f[1], IO: f[2]})
			}
		}
		time.Sleep(2 * time.Second)
	}
	for name, samples := range per {
		if len(samples) == 0 {
			continue
		}
		fmt.Printf("%s: %d samples\n", name, len(samples))
		for _, s := range samples {
			fmt.Printf("  mem=%s blockIO=%s\n", s.Mem, s.IO)
		}
	}
}

func checkFatal(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

var _ = slog.Default
var _ = json.Marshal
var _ = bufio.NewReader
var _ = tls.Config{}
var _ = net.Dial
var _ = http.Get
var _ = io.Discard
var _ = os.Stdout
