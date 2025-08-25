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
// both stacks (docs/benchmark.md). Flags may be placed before or after the
// subcommand; the first non-flag argument is the mode.
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
	"regexp"
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
	engine     = flag.String("engine", "engine", "label for results")
	smtpAddr   = flag.String("smtp", "127.0.0.1:1587", "SMTP submission address")
	inSmtp     = flag.String("in-smtp", "127.0.0.1:25", "inbound SMTP address")
	imapAddr   = flag.String("imap", "127.0.0.1:143", "IMAP address")
	user       = flag.String("user", "u00001@example.com", "auth user")
	pass       = flag.String("pass", "benchpass", "auth password")
	conns      = flag.Int("conns", 300, "concurrent connections")
	msgs       = flag.Int("msgs", 1000, "total messages")
	size       = flag.Int("size", 4096, "message body size (bytes)")
	users      = flag.Int("users", 1, "distinct users to cycle for IMAP sessions")
	to         = flag.String("to", "u%05d@example.com", "recipient format (%%05d = 1..N users)")
	fromDom    = flag.String("from-dom", "external.test", "seed sender domain")
	dur        = flag.Duration("dur", 15*time.Second, "sampling duration for stats")
	mxAddr     = flag.String("mx", "127.0.0.1:25252", "mock MX listen address (queue mode)")
	wait       = flag.Duration("wait", 3*time.Minute, "max wait for queue delivery (queue mode)")
	extDom     = flag.String("ext-dom", "queuebench.test", "external recipient domain (queue mode)")
	containers = flag.String("containers", "", "comma-separated container names (stats mode)")
	seqFlag    = flag.Bool("seq", false, "cycle seed recipients sequentially (u00001..) instead of random")
	verbose    = flag.Bool("verbose", false, "print first failures")
)

var seqN atomic.Int64

func main() {
	normalizeArgs()
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
	case "queue":
		queueBench()
	case "stats":
		stats()
	default:
		fmt.Fprintf(os.Stderr, "bench: unknown mode %q\n", flag.Arg(0))
		os.Exit(2)
	}
}

// valueFlags are the flags that consume an argument; used to locate the
// subcommand when flags are written after it.
var valueFlags = map[string]bool{
	"engine": true, "smtp": true, "in-smtp": true, "imap": true, "user": true,
	"pass": true, "conns": true, "msgs": true, "size": true, "to": true,
	"from-dom": true, "dur": true, "mx": true, "wait": true, "ext-dom": true,
	"containers": true,
	"users":      true,
}

// normalizeArgs moves the first non-flag argument (the mode) to the end so
// flag.Parse sees every flag regardless of whether it was written before or
// after the mode: `bench seed -conns 20` and `bench -conns 20 seed` parse the
// same. Values of value-taking flags are skipped so they are not mistaken for
// the mode.
func normalizeArgs() {
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			name := strings.TrimLeft(a, "-")
			if strings.Contains(name, "=") {
				continue // -flag=value never consumes the next arg
			}
			if valueFlags[name] && i+1 < len(args) {
				i++
			}
			continue
		}
		rest := append([]string{}, args[:i]...)
		rest = append(rest, args[i+1:]...)
		os.Args = append([]string{os.Args[0]}, append(rest, a)...)
		return
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
// A non-empty msgID overrides the generated Message-ID so queue mode can
// correlate deliveries.
func randomBody(subject string, size int, msgID string) []byte {
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
	if msgID == "" {
		msgID = fmt.Sprintf("%d-%d", time.Now().UnixNano(), randInt())
	}
	if !strings.Contains(msgID, "@") {
		msgID += "@bench.test"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "From: sender@bench.test\r\nTo: %s\r\nSubject: %s\r\nMessage-ID: <%s>\r\nDate: %s\r\n\r\n",
		*user, subject, msgID, time.Now().Format(time.RFC1123Z))
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

// formatRecipient fills a %d-style recipient template; a literal address is
// returned unchanged.
func formatRecipient(tmpl string) string {
	if !strings.Contains(tmpl, "%") {
		return tmpl
	}
	if *seqFlag {
		return fmt.Sprintf(tmpl, seqN.Add(1)%10000+1)
	}
	return fmt.Sprintf(tmpl, randUserID())
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
		wg        sync.WaitGroup
		start     = time.Now()
		done      atomic.Int64
		fails     atomic.Int64
		firstErrs []string
		latMu     sync.Mutex
		latency   []time.Duration
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
	body := randomBody("bench seed", *size, "")
	res := runConcurrent(*conns, *msgs, func() error {
		c, err := gosmtp.Dial(*inSmtp)
		if err != nil {
			return err
		}
		defer c.Close()
		to := formatRecipient(*to)
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
	body := randomBody("bench smtp", *size, "")
	res := runConcurrent(*conns, *msgs, func() error {
		c, err := gosmtp.Dial(*smtpAddr)
		if err != nil {
			return err
		}
		defer c.Close()
		sender := nextUser()
		if err := c.Auth(sasl.NewPlainClient("", sender, *pass)); err != nil {
			return err
		}
		to := formatRecipient(*to)
		if err := c.Mail(sender, nil); err != nil {
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
			if err := ic.Login(benchUser(w), *pass).Wait(); err != nil {
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
				_, err := ic.Fetch(imap.SeqSetNum(1), &imap.FetchOptions{Envelope: true}).Collect()
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

// benchUser returns the user for worker/index i: -user when -users=1,
// otherwise u%05d on the same domain, cycling 1..-users so per-user limits
// (legacy IMAP mail_max_userip_connections, backend per-sender rate limits) do
// not skew concurrent results.
func benchUser(i int) string {
	if *users <= 1 {
		return *user
	}
	at := strings.LastIndex(*user, "@")
	domain := *user
	if at >= 0 {
		domain = (*user)[at+1:]
	}
	return fmt.Sprintf("u%05d@%s", i%*users+1, domain)
}

var senderN atomic.Int64

// nextUser rotates through the -users range for per-message senders.
func nextUser() string {
	if *users <= 1 {
		return *user
	}
	return benchUser(int(senderN.Add(1)))
}

// queueBench drives outbound queue performance: authenticated submissions to
// an external domain while a mock MX (started by this mode) receives them.
// The engine must be configured to relay that domain to -mx (relayhost /
// fixed outbound host). Per-message Message-IDs correlate the DATA accept
// timestamp with the mock MX receipt so enqueue→deliver latency is exact.
func queueBench() {
	rec := &mxRecorder{times: map[string]time.Time{}}
	srv := gosmtp.NewServer(&mxBackend{rec: rec})
	srv.Domain = "mock-mx"
	srv.MaxMessageBytes = 1 << 22
	srv.ReadTimeout = 60 * time.Second
	srv.WriteTimeout = 60 * time.Second
	ln, err := net.Listen("tcp", *mxAddr)
	checkFatal(err)
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()
	// Confirm the mock MX is accepting before any submission.
	probe, err := net.DialTimeout("tcp", *mxAddr, 5*time.Second)
	checkFatal(err)
	_ = probe.Close()

	type submit struct {
		id       string
		accepted time.Time
		err      error
	}
	var (
		wg       sync.WaitGroup
		seq      atomic.Int64
		submits  = make([]submit, 0, *msgs)
		latMu    sync.Mutex
		fails    atomic.Int64
		firstErr []string
	)
	work := make(chan struct{}, *msgs)
	for i := 0; i < *msgs; i++ {
		work <- struct{}{}
	}
	close(work)
	start := time.Now()
	for w := 0; w < *conns; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range work {
				id := fmt.Sprintf("q%d-%d@bench.test", seq.Add(1), randInt())
				body := randomBody("bench queue", *size, id)
				t0 := time.Now()
				err := submitOne(*smtpAddr, id, body)
				latMu.Lock()
				submits = append(submits, submit{id: id, accepted: t0, err: err})
				latMu.Unlock()
				if err != nil {
					fails.Add(1)
					if *verbose && int(fails.Load()) <= 5 {
						latMu.Lock()
						firstErr = append(firstErr, err.Error())
						latMu.Unlock()
					}
				}
			}
		}()
	}
	wg.Wait()
	subElapsed := time.Since(start)
	successes := 0
	latMu.Lock()
	for _, s := range submits {
		if s.err == nil {
			successes++
		}
	}
	latMu.Unlock()
	fmt.Printf("\n== %s queue\n", *engine)
	fmt.Printf("  submitted=%d conns=%d duration=%.2fs rate=%.1f msg/s failures=%d\n",
		successes, *conns, subElapsed.Seconds(), float64(successes)/subElapsed.Seconds(), fails.Load())

	// Wait for the queue to drain into the mock MX, sampling backlog.
	var (
		received int
		maxBk    = 0
		delivery []time.Duration
	)
	deadline := time.Now().Add(*wait)
	for time.Now().Before(deadline) && received < successes {
		time.Sleep(time.Second)
		received = rec.count()
		latMu.Lock()
		bk := successes - received
		latMu.Unlock()
		if bk > maxBk {
			maxBk = bk
		}
	}
	received = rec.count()
	latMu.Lock()
	for _, s := range submits {
		if s.err != nil {
			continue
		}
		if t, ok := rec.receipt(s.id); ok {
			delivery = append(delivery, t.Sub(s.accepted))
		}
	}
	latMu.Unlock()
	deliverRate := float64(received) / time.Since(start).Seconds()
	fmt.Printf("  delivered=%d rate=%.1f msg/s (%.0f%% of accepted)\n",
		received, deliverRate, 100*float64(received)/float64(maxInt(1, successes)))
	if len(delivery) > 0 {
		fmt.Printf("  enqueue→deliver p50=%s p95=%s p99=%s max=%s\n",
			percentiles(delivery, 0.50), percentiles(delivery, 0.95),
			percentiles(delivery, 0.99), percentiles(delivery, 1.0))
	}
	fmt.Printf("  max queue backlog=%d (submitted-delivered)\n", maxBk)
	if *verbose {
		for _, e := range firstErr {
			fmt.Printf("  fail: %v\n", e)
		}
	}
}

// submitOne sends one message from the -user account to an external recipient
// on -ext-dom and returns the DATA result.
func submitOne(addr, msgID string, body []byte) error {
	c, err := gosmtp.Dial(addr)
	if err != nil {
		return err
	}
	defer c.Close()
	sender := nextUser()
	if err := c.Auth(sasl.NewPlainClient("", sender, *pass)); err != nil {
		return err
	}
	to := fmt.Sprintf("ext%d@%s", randInt()%1000000, *extDom)
	if err := c.Mail(sender, nil); err != nil {
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
}

// mxRecorder records mock-MX receipts keyed by Message-ID.
type mxRecorder struct {
	mu    sync.Mutex
	times map[string]time.Time
}

func (m *mxRecorder) record(body []byte) {
	id := extractMsgID(body)
	m.mu.Lock()
	defer m.mu.Unlock()
	if id != "" {
		m.times[id] = time.Now()
	}
}

func (m *mxRecorder) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.times)
}

func (m *mxRecorder) receipt(id string) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.times[id]
	return t, ok
}

var msgIDRe = regexp.MustCompile(`(?i)^Message-ID:\s*<([^>]+)>`)

func extractMsgID(body []byte) string {
	for _, line := range strings.Split(string(body), "\r\n") {
		if m := msgIDRe.FindStringSubmatch(line); m != nil {
			return m[1]
		}
		if line == "" {
			return ""
		}
	}
	return ""
}

// mxBackend accepts everything and records each DATA receipt.
type mxBackend struct {
	rec *mxRecorder
}

func (b *mxBackend) NewSession(_ *gosmtp.Conn) (gosmtp.Session, error) {
	return &mxSession{b: b}, nil
}

type mxSession struct {
	b *mxBackend
}

func (s *mxSession) Mail(string, *gosmtp.MailOptions) error { return nil }
func (s *mxSession) Rcpt(string, *gosmtp.RcptOptions) error { return nil }
func (s *mxSession) Data(r io.Reader) error {
	body, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.b.rec.record(body)
	return nil
}
func (s *mxSession) Reset()        {}
func (s *mxSession) Logout() error { return nil }

// stats samples container memory and block IO via docker stats.
func stats() {
	if *containers == "" {
		fmt.Fprintln(os.Stderr, "usage: bench -containers c1,c2 stats")
		os.Exit(2)
	}
	names := strings.Split(*containers, ",")
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

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

var _ = slog.Default
var _ = json.Marshal
var _ = bufio.NewReader
var _ = tls.Config{}
var _ = net.Dial
var _ = http.Get
var _ = io.Discard
var _ = os.Stdout
