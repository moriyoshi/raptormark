// Copyright 2026 The raptormark Authors
// SPDX-License-Identifier: LGPL-2.1-or-later

package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// session drives a whole stdio conversation and returns one decoded response
// per line the server emitted.
//
// Deliberately at the PROTOCOL level rather than calling the tool functions
// directly. The failure modes that matter here are framing ones -- a stray
// write to stdout, an embedded newline, a response to a notification, a missing
// id -- and none of them are visible to a test that calls Go functions.
func session(t *testing.T, lines ...string) []map[string]any {
	t.Helper()
	var out, logbuf strings.Builder
	s, err := New(strings.NewReader(strings.Join(lines, "\n")+"\n"), &out, &logbuf, "objdump")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Serve(); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	raw := out.String()
	if raw == "" {
		return nil
	}
	if !strings.HasSuffix(raw, "\n") {
		t.Errorf("stream does not end with a newline; a client would block on the last message")
	}
	var msgs []map[string]any
	for i, l := range strings.Split(strings.TrimSuffix(raw, "\n"), "\n") {
		if l == "" {
			t.Errorf("message %d is an empty line", i)
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("message %d is not valid JSON: %v\n%s", i, err, l)
		}
		msgs = append(msgs, m)
	}
	return msgs
}

const initReq = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`
const initedNote = `{"jsonrpc":"2.0","method":"notifications/initialized"}`

func TestInitializeHandshake(t *testing.T) {
	msgs := session(t, initReq, initedNote)

	// The notification must NOT produce a response. A server that answers it
	// desynchronises a client's id accounting.
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1 (the notification must not be answered): %v", len(msgs), msgs)
	}
	m := msgs[0]
	if m["jsonrpc"] != "2.0" {
		t.Errorf("jsonrpc = %v", m["jsonrpc"])
	}
	if m["id"] != float64(1) {
		t.Errorf("id = %v, want 1", m["id"])
	}
	res, ok := m["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", m)
	}
	if res["protocolVersion"] != protocolVersion {
		t.Errorf("protocolVersion = %v, want %s", res["protocolVersion"], protocolVersion)
	}
	caps, _ := res["capabilities"].(map[string]any)
	if _, ok := caps["tools"]; !ok {
		t.Errorf("tools capability not declared: %v", caps)
	}
	info, _ := res["serverInfo"].(map[string]any)
	if info["name"] != serverName {
		t.Errorf("serverInfo.name = %v", info["name"])
	}
	// The instructions carry the alias and padding caveats; losing them is a
	// silent downgrade in answer quality rather than a failure, so pin them.
	instr, _ := res["instructions"].(string)
	for _, want := range []string{"ALIAS", "0x00000000"} {
		if !strings.Contains(instr, want) {
			t.Errorf("instructions no longer mention %q", want)
		}
	}
}

// TestVersionNegotiationAnswersWithOurs: the spec says respond with the client's
// version if supported, otherwise one we support -- NOT an error.
func TestVersionNegotiationAnswersWithOurs(t *testing.T) {
	req := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`
	msgs := session(t, req)
	if len(msgs) != 1 {
		t.Fatalf("got %d messages", len(msgs))
	}
	if _, isErr := msgs[0]["error"]; isErr {
		t.Fatalf("unknown version was rejected; the spec says answer with a supported one: %v", msgs[0])
	}
	res := msgs[0]["result"].(map[string]any)
	if res["protocolVersion"] != protocolVersion {
		t.Errorf("protocolVersion = %v, want %s", res["protocolVersion"], protocolVersion)
	}
}

func TestToolsListShape(t *testing.T) {
	msgs := session(t, initReq, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	res := msgs[1]["result"].(map[string]any)
	tools, _ := res["tools"].([]any)
	if len(tools) != 3 {
		t.Fatalf("got %d tools, want 3", len(tools))
	}
	seen := map[string]bool{}
	for _, ti := range tools {
		tm := ti.(map[string]any)
		name, _ := tm["name"].(string)
		seen[name] = true
		if tm["description"] == "" || tm["description"] == nil {
			t.Errorf("%s has no description", name)
		}
		sch, ok := tm["inputSchema"].(map[string]any)
		if !ok {
			t.Fatalf("%s has no inputSchema", name)
		}
		if sch["type"] != "object" {
			t.Errorf("%s inputSchema.type = %v, want object", name, sch["type"])
		}
		if _, ok := sch["properties"]; !ok {
			t.Errorf("%s inputSchema has no properties", name)
		}
		if _, ok := sch["required"]; !ok {
			t.Errorf("%s inputSchema declares nothing required", name)
		}
	}
	for _, want := range []string{"decode_encoding", "decode_report", "decode_corpus"} {
		if !seen[want] {
			t.Errorf("tool %s missing", want)
		}
	}
}

func call(name, args string) string {
	return `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}`
}

func TestDecodeEncodingTool(t *testing.T) {
	msgs := session(t, initReq, call("decode_encoding",
		`{"encodings":["0x4c9f7000","4e020020","0x00000000"]}`))
	res := msgs[1]["result"].(map[string]any)
	if res["isError"] == true {
		t.Fatalf("tool reported an error: %v", res)
	}

	content := res["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)
	// Real encodings, verified against objdump elsewhere in this module.
	for _, want := range []string{
		"ST_mult", "rpt=1", "selem=1", // 0x4c9f7000
		"TBL_TBX", "len=0", // 0x4e020020, accepted without the 0x prefix
		"padding lifted as code", // 0x00000000 must be explained, not just unmatched
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text result is missing %q:\n%s", want, text)
		}
	}

	sc, ok := res["structuredContent"].(map[string]any)
	if !ok {
		t.Fatal("no structuredContent")
	}
	ls, _ := sc["lookups"].([]any)
	if len(ls) != 3 {
		t.Fatalf("got %d lookups, want 3 (one per input, in order)", len(ls))
	}
	first := ls[0].(map[string]any)
	if first["pattern"] != "ST_mult" {
		t.Errorf("lookups[0].pattern = %v", first["pattern"])
	}
	if last := ls[2].(map[string]any); last["matched"] != false {
		t.Errorf("the all-zero word must report matched=false, got %v", last["matched"])
	}
}

// TestBadArgumentsAreToolErrorsNotProtocolErrors: the spec separates the two,
// and the distinction is load-bearing -- a tool error is something the model can
// see and retry, a protocol error is a client-level fault.
func TestBadArgumentsAreToolErrors(t *testing.T) {
	for _, c := range []struct{ name, args, want string }{
		{"decode_encoding", `{"encodings":["zzzz"]}`, "not a 32-bit hex encoding"},
		{"decode_encoding", `{"encodings":[]}`, "no encodings given"},
		{"decode_report", `{"path":"/nonexistent/nope.log"}`, "no such file"},
		{"decode_corpus", `{"path":"/nonexistent/nope.elf"}`, "no such file"},
	} {
		msgs := session(t, initReq, call(c.name, c.args))
		m := msgs[1]
		if _, isProto := m["error"]; isProto {
			t.Errorf("%s %s: reported as a PROTOCOL error; bad input is a tool error", c.name, c.args)
			continue
		}
		res := m["result"].(map[string]any)
		if res["isError"] != true {
			t.Errorf("%s %s: isError not set: %v", c.name, c.args, res)
			continue
		}
		text := res["content"].([]any)[0].(map[string]any)["text"].(string)
		if !strings.Contains(strings.ToLower(text), c.want) {
			t.Errorf("%s %s: message %q does not mention %q", c.name, c.args, text, c.want)
		}
	}
}

// TestUnknownToolIsAProtocolError is the other side of the same distinction.
func TestUnknownToolIsAProtocolError(t *testing.T) {
	msgs := session(t, initReq, call("no_such_tool", `{}`))
	e, ok := msgs[1]["error"].(map[string]any)
	if !ok {
		t.Fatalf("unknown tool did not produce a protocol error: %v", msgs[1])
	}
	if e["code"] != float64(codeInvalidParams) {
		t.Errorf("code = %v, want %d", e["code"], codeInvalidParams)
	}
}

func TestUnknownMethodAndMalformedInput(t *testing.T) {
	msgs := session(t,
		`{"jsonrpc":"2.0","id":1,"method":"nope/nope"}`,
		`{not json at all`,
		`{"jsonrpc":"2.0","method":"notifications/unknown"}`, // must be ignored silently
		`{"jsonrpc":"2.0","id":4,"method":"ping"}`,
	)
	if len(msgs) != 3 {
		t.Fatalf("got %d messages, want 3 (unknown method, parse error, ping): %v", len(msgs), msgs)
	}
	if e := msgs[0]["error"].(map[string]any); e["code"] != float64(codeMethodNotFound) {
		t.Errorf("unknown method code = %v, want %d", e["code"], codeMethodNotFound)
	}
	if e := msgs[1]["error"].(map[string]any); e["code"] != float64(codeParseError) {
		t.Errorf("parse error code = %v, want %d", e["code"], codeParseError)
	}
	if msgs[1]["id"] != nil {
		t.Errorf("a parse error must carry a null id, got %v", msgs[1]["id"])
	}
	if _, ok := msgs[2]["result"]; !ok {
		t.Errorf("ping did not return a result: %v", msgs[2])
	}
}

// TestNoEmbeddedNewlines guards the framing rule directly: "Messages are
// delimited by newlines, and MUST NOT contain embedded newlines."
//
// The report output is full of newlines, so this is a live hazard rather than a
// theoretical one -- it passes only because json.Marshal escapes them.
func TestNoEmbeddedNewlines(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "t.log")
	if err := os.WriteFile(log, []byte(
		"[ecv-undecoded] vma=0x1000 enc=0x4c9f7000 fn=0x1000\n"+
			"[ecv-undecoded] vma=0x1004 enc=0x4e020020 fn=0x1000\n"+
			"[ecv-undecoded] vma=0x1008 enc=0x00000000 fn=0x1000\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, logbuf strings.Builder
	in := strings.Join([]string{initReq, call("decode_report", `{"path":"`+log+`"}`)}, "\n") + "\n"
	s, err := New(strings.NewReader(in), &out, &logbuf, "objdump")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Serve(); err != nil {
		t.Fatal(err)
	}

	raw := strings.TrimSuffix(out.String(), "\n")
	if n := strings.Count(raw, "\n"); n != 1 {
		t.Fatalf("expected exactly 2 messages (1 separator), found %d separators -- "+
			"a multi-line payload leaked into the frame", n)
	}
	// And the payload really did contain newlines, or this proves nothing.
	last := raw[strings.LastIndex(raw, "\n")+1:]
	var m map[string]any
	if err := json.Unmarshal([]byte(last), &m); err != nil {
		t.Fatal(err)
	}
	text := m["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "\n") {
		t.Fatal("the report payload has no newlines, so this test proves nothing")
	}
	if !strings.Contains(text, "ST_mult") || !strings.Contains(text, "1 padding sites") {
		t.Errorf("report payload looks wrong:\n%s", text)
	}
}

// TestServerRefusesInvalidTables: New validates at startup so a bad pin fails
// the launch rather than answering every call confidently and wrongly. There is
// no way to inject a broken table here, so this asserts the coupling exists --
// New must consult Validate, not merely Parse.
func TestServerValidatesAtStartup(t *testing.T) {
	var out, logbuf strings.Builder
	s, err := New(strings.NewReader(""), &out, &logbuf, "objdump")
	if err != nil {
		t.Fatalf("startup failed on the shipped tables: %v", err)
	}
	if s.dec == nil {
		t.Fatal("no decoder built")
	}
	if probs := s.dec.Validate(); len(probs) != 0 {
		t.Fatalf("shipped tables do not validate: %d problems", len(probs))
	}
}
