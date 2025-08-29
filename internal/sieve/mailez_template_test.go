// The mailez control plane's default Sieve template must compile and
// execute on the mailezine engine. This test pins that contract with a
// faithful copy of the template, exercising every extension the template
// requires.
package sieve

import (
	"context"
	"strings"
	"testing"
)

const mailezTemplate = `require "variables";
require "vacation";
require "fileinto";
require "envelope";
require "mailbox";
require "imap4flags";
require "regex";
require "relational";
require "date";
require "comparator-i;ascii-numeric";
require "spamtestplus";
require "editheader";
require "index";

if header :index 2 :matches "Received" "from * by * for <*>; *"
{
  deleteheader "Delivered-To";
  addheader "Delivered-To" "<${3}>";
}

if spamtest :percent :value "gt" :comparator "i;ascii-numeric" "85"
{
  setflag "\seen";
  fileinto :create "Junk";
  stop;
}

if not address :localpart :contains ["From","Reply-To"] ["noreply","no-reply"]{
  vacation :days 1 :from "alice@example.com" :subject "away" "on vacation";
}
`

func routeTemplate(t *testing.T, src, msg string) Result {
	t.Helper()
	e := NewEngine(nil)
	res, err := e.Route(context.Background(), src, "sender@remote.test", "alice@example.com", []byte(msg))
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	return res
}

func TestMailezTemplateCompiles(t *testing.T) {
	msg := "Received: from mx (10.0.0.1) by mail (10.0.0.2) for <alice@example.com>; Mon, 1 Jan 2026 10:00:00 +0000\r\n" +
		"Received: from gateway (10.0.0.9) by mail (10.0.0.2) for <alice@example.com>; Mon, 1 Jan 2026 10:00:01 +0000\r\n" +
		"From: sender@remote.test\r\nTo: alice@example.com\r\nSubject: hi\r\n\r\nbody\r\n"
	res := routeTemplate(t, mailezTemplate, msg)
	if len(res.DeleteHeaders) != 1 || res.DeleteHeaders[0].Name != "Delivered-To" {
		t.Fatalf("deleteheader not collected: %+v", res.DeleteHeaders)
	}
	if len(res.AddHeaders) != 1 || res.AddHeaders[0].Name != "Delivered-To" || res.AddHeaders[0].Value != "<alice@example.com>" {
		t.Fatalf("addheader not collected: %+v", res.AddHeaders)
	}
	if res.Vacation == nil || res.Vacation.Subject != "away" || res.Vacation.Body != "on vacation" {
		t.Fatalf("vacation not collected: %+v", res.Vacation)
	}
}

func TestMailezTemplateSpamJunk(t *testing.T) {
	// 13 stars = 86% > 85% -> Junk.
	msg := "From: spam@remote.test\r\n" +
		"X-Spam-Level: *************\r\n" +
		"Subject: viagra\r\n\r\nbuy now\r\n"
	res := routeTemplate(t, mailezTemplate, msg)
	if len(res.Mailboxes) != 1 || res.Mailboxes[0] != "Junk" {
		t.Fatalf("spam not filed to Junk: %+v", res.Mailboxes)
	}
	if !hasFlag(res.Flags, "\\seen") {
		t.Fatalf("spam not marked \\seen: %v", res.Flags)
	}
	if res.Vacation != nil {
		t.Fatal("vacation must be skipped when spam stops the script")
	}
}

func TestMailezTemplateSpamClean(t *testing.T) {
	msg := "From: sender@remote.test\r\n" +
		"X-Spam-Level: **\r\n" +
		"Subject: hello\r\n\r\nbody\r\n"
	res := routeTemplate(t, mailezTemplate, msg)
	if len(res.Mailboxes) != 1 || res.Mailboxes[0] != "INBOX" {
		t.Fatalf("clean message not kept: %+v", res.Mailboxes)
	}
	if res.Vacation == nil {
		t.Fatal("clean message should trigger vacation")
	}
}

func TestMailezTemplateNoReplyNoVacation(t *testing.T) {
	msg := "From: noreply@remote.test\r\nSubject: receipt\r\n\r\nok\r\n"
	res := routeTemplate(t, mailezTemplate, msg)
	if res.Vacation != nil {
		t.Fatal("noreply sender must not get a vacation reply")
	}
}

func TestSpamTestRelational(t *testing.T) {
	// Boundary: 12 stars = 80% not > 85%.
	msg := "From: x@y.test\r\nX-Spam-Level: ************\r\nSubject: s\r\n\r\nb\r\n"
	res := routeTemplate(t, mailezTemplate, msg)
	if len(res.Mailboxes) != 1 || res.Mailboxes[0] != "INBOX" {
		t.Fatalf("80%% should not file to Junk: %+v", res.Mailboxes)
	}
}

func hasFlag(flags []string, want string) bool {
	for _, f := range flags {
		if strings.EqualFold(f, want) {
			return true
		}
	}
	return false
}
