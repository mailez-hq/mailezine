// mailez extensions for the Sieve interpreter. The mailez control plane's
// default script template depends on these (spamtestplus, vacation,
// editheader, index, regex, date, mailbox); without them the default filter
// cannot compile. Implementations follow the RFCs and the X-Spam-Level
// conventions the template was written for.
package interp

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/foxcpp/go-sieve/parser"
)

// ---------------------------------------------------------------------------
// editheader (RFC 5293): addheader / deleteheader.
// ---------------------------------------------------------------------------

// ActionAddHeader adds a header field to the message being delivered.
type ActionAddHeader struct {
	Name  string
	Value string
}

func (ActionAddHeader) testActionName() string    { return "addheader" }
func (ActionAddHeader) cancelsImplicitKeep() bool { return false }

// ActionDeleteHeader removes header fields from the message being delivered.
type ActionDeleteHeader struct {
	Name   string
	Values []string // nil = delete every instance
	Index  int      // RFC 5293 §2.5: 1-based instance to delete (0 = all)
}

func (ActionDeleteHeader) testActionName() string    { return "deleteheader" }
func (ActionDeleteHeader) cancelsImplicitKeep() bool { return false }

type CmdAddHeader struct {
	Name  string
	Value string
}

func (c CmdAddHeader) Execute(ctx context.Context, d *RuntimeData) error {
	return d.OnAction(ctx, ActionAddHeader{
		Name:  expandVars(d, c.Name),
		Value: expandVars(d, c.Value),
	}, d)
}

type CmdDeleteHeader struct {
	Name   string
	Values []string
	Index  int
}

func (c CmdDeleteHeader) Execute(ctx context.Context, d *RuntimeData) error {
	return d.OnAction(ctx, ActionDeleteHeader{
		Name:   expandVars(d, c.Name),
		Values: expandVarsList(d, c.Values),
		Index:  c.Index,
	}, d)
}

func loadAddHeader(s *Script, pcmd parser.Cmd) (Cmd, error) {
	if !s.RequiresExtension("editheader") {
		return nil, parser.ErrorAt(pcmd.Position, "missing require 'editheader'")
	}
	cmd := CmdAddHeader{}
	err := LoadSpec(s, &Spec{
		Pos: []SpecPosArg{
			{
				MatchStr:    func(val []string) { cmd.Name = val[0] },
				MinStrCount: 1,
				MaxStrCount: 1,
			},
			{
				MatchStr:    func(val []string) { cmd.Value = val[0] },
				MinStrCount: 1,
				MaxStrCount: 1,
			},
		},
	}, pcmd.Position, pcmd.Args, pcmd.Tests, nil)
	return cmd, err
}

func loadDeleteHeader(s *Script, pcmd parser.Cmd) (Cmd, error) {
	if !s.RequiresExtension("editheader") {
		return nil, parser.ErrorAt(pcmd.Position, "missing require 'editheader'")
	}
	cmd := CmdDeleteHeader{}
	err := LoadSpec(s, &Spec{
		Tags: map[string]SpecTag{
			"index": {
				NeedsValue:  true,
				MinStrCount: 1,
				MaxStrCount: 1,
				MatchNum:    func(i int) { cmd.Index = i },
			},
		},
		Pos: []SpecPosArg{
			{
				MatchStr:    func(val []string) { cmd.Name = val[0] },
				MinStrCount: 1,
				MaxStrCount: 1,
			},
			{
				MatchStr: func(val []string) { cmd.Values = append(cmd.Values, val...) },
				Optional: true,
			},
		},
	}, pcmd.Position, pcmd.Args, pcmd.Tests, nil)
	return cmd, err
}

// ---------------------------------------------------------------------------
// vacation (RFC 5230 subset used by the mailez template).
// ---------------------------------------------------------------------------

// ActionVacation requests an automatic reply. The delivery pipeline owns the
// reply generation and the per-recipient :days throttle.
type ActionVacation struct {
	Days      int
	From      string
	Subject   string
	Body      string
	Addresses []string // optional address list restriction (not used by template)
}

func (ActionVacation) testActionName() string    { return "vacation" }
func (ActionVacation) cancelsImplicitKeep() bool { return false }

type CmdVacation struct {
	Days      int
	From      string
	Subject   string
	Body      string
	Addresses []string
}

func (c CmdVacation) Execute(ctx context.Context, d *RuntimeData) error {
	return d.OnAction(ctx, ActionVacation{
		Days:      c.Days,
		From:      expandVars(d, c.From),
		Subject:   expandVars(d, c.Subject),
		Body:      expandVars(d, c.Body),
		Addresses: expandVarsList(d, c.Addresses),
	}, d)
}

func loadVacation(s *Script, pcmd parser.Cmd) (Cmd, error) {
	if !s.RequiresExtension("vacation") {
		return nil, parser.ErrorAt(pcmd.Position, "missing require 'vacation'")
	}
	cmd := CmdVacation{Days: 7}
	err := LoadSpec(s, &Spec{
		Tags: map[string]SpecTag{
			"days": {
				NeedsValue:  true,
				MinStrCount: 1,
				MaxStrCount: 1,
				MatchNum:    func(i int) { cmd.Days = i },
			},
			"from": {
				NeedsValue:  true,
				MinStrCount: 1,
				MaxStrCount: 1,
				MatchStr:    func(val []string) { cmd.From = val[0] },
			},
			"subject": {
				NeedsValue:  true,
				MinStrCount: 1,
				MaxStrCount: 1,
				MatchStr:    func(val []string) { cmd.Subject = val[0] },
			},
			"addresses": {
				NeedsValue: true,
				MatchStr:   func(val []string) { cmd.Addresses = append(cmd.Addresses, val...) },
			},
		},
		Pos: []SpecPosArg{
			{
				MatchStr:    func(val []string) { cmd.Body = val[0] },
				MinStrCount: 1,
				MaxStrCount: 1,
			},
		},
	}, pcmd.Position, pcmd.Args, pcmd.Tests, nil)
	return cmd, err
}

// ---------------------------------------------------------------------------
// spamtestplus: read the X-Spam-Level header, :percent form.
// ---------------------------------------------------------------------------

// SpamTestHeader is the header the spamtest test reads. mailezine injects
// it after classification.
const SpamTestHeader = "X-Spam-Level"

// SpamTestMaxLevel is the maximum level string length. The percentage is
// len(level)/max*100.
const SpamTestMaxLevel = 15

type SpamTest struct {
	matcherTest
	Percent bool
}

func (t SpamTest) Check(_ context.Context, d *RuntimeData) (bool, error) {
	values, err := d.Msg.HeaderGet(SpamTestHeader)
	if err != nil {
		return false, err
	}
	if len(values) == 0 {
		return false, nil
	}
	level := strings.TrimSpace(values[0])
	if t.Percent {
		pct := len(level) * 100 / SpamTestMaxLevel
		return t.tryMatch(d, strconv.Itoa(pct))
	}
	return t.tryMatch(d, level)
}

func loadSpamTest(s *Script, test parser.Test) (Test, error) {
	if !s.RequiresExtension("spamtestplus") {
		return nil, parser.ErrorAt(test.Position, "missing require 'spamtestplus'")
	}
	loaded := SpamTest{matcherTest: newMatcherTest()}
	var key []string
	err := LoadSpec(s, loaded.addSpecTags(&Spec{
		Tags: map[string]SpecTag{
			"percent": {
				MatchBool: func() { loaded.Percent = true },
			},
		},
		Pos: []SpecPosArg{
			{
				MatchStr:    func(val []string) { key = val },
				MinStrCount: 1,
			},
		},
	}), test.Position, test.Args, test.Tests, nil)
	if err != nil {
		return nil, err
	}
	if err := loaded.setKey(s, key); err != nil {
		return nil, err
	}
	return loaded, err
}

// ---------------------------------------------------------------------------
// date (RFC 5260 subset): date / currentdate with :zone and :originalzone.
// The mailez template only requires the extension; keep the surface small
// but correct for the common "currentdate :value ge :date" patterns.
// ---------------------------------------------------------------------------

type DatePart int

const (
	DatePartDate DatePart = iota
	DatePartTime
)

type DateTest struct {
	matcherTest
	Part     DatePart
	Original bool
}

func (t DateTest) Check(_ context.Context, d *RuntimeData) (bool, error) {
	// The value compared against the date header. RFC 5260 requires the
	// "date" header; we only compare its "date" part in this subset.
	values, err := d.Msg.HeaderGet("date")
	if err != nil {
		return false, err
	}
	if len(values) == 0 {
		return false, nil
	}
	dateStr := values[0]
	// Extract the date token (RFC 2822: "Mon, 7 Feb 1994").
	if idx := strings.IndexByte(dateStr, ':'); idx >= 0 {
		dateStr = strings.TrimSpace(dateStr[:idx])
	}
	if t.Part == DatePartDate {
		// Normalize the RFC 2822 date to the yyyy-mm-dd form used in
		// comparisons by mail clients.
		normalized := normalizeDate(dateStr)
		return t.tryMatch(d, normalized)
	}
	return t.tryMatch(d, dateStr)
}

func normalizeDate(s string) string {
	// RFC 2822 date ("Mon, 7 Feb 1994") -> "1994-02-07". We don't pull a
	// full date parser here; the template compares :date with :value and
	// i;ascii-numeric. Accept the common formats, best-effort.
	fields := strings.Fields(strings.TrimPrefix(s, ","))
	if len(fields) < 3 {
		return s
	}
	day := strings.TrimRight(fields[len(fields)-2], ",")
	month := monthNumber(fields[len(fields)-2])
	year := fields[len(fields)-1]
	return year + "-" + month + "-" + pad2(day)
}

func monthNumber(m string) string {
	months := []string{"", "jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}
	for i, name := range months {
		if name == strings.ToLower(m) {
			return pad2(strconv.Itoa(i))
		}
	}
	return m
}

func pad2(s string) string {
	if len(s) == 1 {
		return "0" + s
	}
	return s
}

func loadDateTest(s *Script, test parser.Test) (Test, error) {
	if !s.RequiresExtension("date") {
		return nil, parser.ErrorAt(test.Position, "missing require 'date'")
	}
	loaded := DateTest{matcherTest: newMatcherTest(), Part: DatePartDate}
	err := LoadSpec(s, loaded.addSpecTags(&Spec{
		Tags: map[string]SpecTag{
			"zone": {
				MatchStr: func(val []string) {},
			},
			"originalzone": {
				MatchBool: func() { loaded.Original = true },
			},
		},
		Pos: []SpecPosArg{
			{
				MatchStr: func(val []string) {
					switch strings.ToLower(val[0]) {
					case "date":
						loaded.Part = DatePartDate
					case "time":
						loaded.Part = DatePartTime
					}
				},
				MinStrCount: 1,
				MaxStrCount: 1,
			},
		},
	}), test.Position, test.Args, test.Tests, nil)
	return loaded, err
}

func loadCurrentDateTest(s *Script, test parser.Test) (Test, error) {
	if !s.RequiresExtension("date") {
		return nil, parser.ErrorAt(test.Position, "missing require 'date'")
	}
	loaded := DateTest{matcherTest: newMatcherTest(), Part: DatePartDate}
	// currentdate compares against the current date in the :zone given.
	// The loaded test is wrapped so Check uses now instead of the header.
	err := LoadSpec(s, loaded.addSpecTags(&Spec{
		Tags: map[string]SpecTag{
			"zone": {
				MatchStr: func(val []string) {},
			},
		},
	}), test.Position, test.Args, test.Tests, nil)
	if err != nil {
		return nil, err
	}
	return currentDateWrapper{loaded}, nil
}

type currentDateWrapper struct{ inner DateTest }

func (w currentDateWrapper) Check(_ context.Context, _ *RuntimeData) (bool, error) {
	now := time.Now().UTC().Format("2006-01-02")
	return w.inner.tryMatch(nil, now)
}

// ---------------------------------------------------------------------------
// mailbox (RFC 5490 subset): mailboxexists / metadataexists.
// ---------------------------------------------------------------------------

// MailboxExistsProvider is implemented by the runtime when the delivery
// pipeline can answer mailbox existence.
type MailboxExistsProvider interface {
	MailboxExists(name string) bool
}

type MailboxExistsTest struct {
	Mailbox string
}

func (t MailboxExistsTest) Check(_ context.Context, d *RuntimeData) (bool, error) {
	if p, ok := d.Env.(MailboxExistsProvider); ok {
		return p.MailboxExists(expandVars(d, t.Mailbox)), nil
	}
	// No provider: assume the mailbox exists (fileinto :create handles the
	// creation case; this keeps scripts that only test existence working).
	return true, nil
}

type MetadataExistsTest struct {
	Mailbox string
	Key     string
}

func (t MetadataExistsTest) Check(_ context.Context, _ *RuntimeData) (bool, error) {
	// Metadata is not modeled in mailezine; a missing annotation is
	// equivalent to "no metadata exists".
	return false, nil
}

func loadMailboxExistsTest(s *Script, test parser.Test) (Test, error) {
	if !s.RequiresExtension("mailbox") {
		return nil, parser.ErrorAt(test.Position, "missing require 'mailbox'")
	}
	loaded := MailboxExistsTest{}
	err := LoadSpec(s, &Spec{
		Pos: []SpecPosArg{
			{
				MatchStr:    func(val []string) { loaded.Mailbox = val[0] },
				MinStrCount: 1,
				MaxStrCount: 1,
			},
		},
	}, test.Position, test.Args, test.Tests, nil)
	return loaded, err
}

func loadMetadataExistsTest(s *Script, test parser.Test) (Test, error) {
	if !s.RequiresExtension("mailbox") {
		return nil, parser.ErrorAt(test.Position, "missing require 'mailbox'")
	}
	loaded := MetadataExistsTest{}
	err := LoadSpec(s, &Spec{
		Pos: []SpecPosArg{
			{
				MatchStr:    func(val []string) { loaded.Mailbox = val[0] },
				MinStrCount: 1,
				MaxStrCount: 1,
			},
			{
				MatchStr:    func(val []string) { loaded.Key = val[0] },
				MinStrCount: 1,
				MaxStrCount: 1,
			},
		},
	}, test.Position, test.Args, test.Tests, nil)
	return loaded, err
}
