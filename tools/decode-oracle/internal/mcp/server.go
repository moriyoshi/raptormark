// Copyright 2026 The raptormark Authors
// SPDX-License-Identifier: LGPL-2.1-or-later

// Package mcp serves the decode oracle over the Model Context Protocol's stdio
// transport, so an agent can call it as a typed tool instead of shelling out
// and parsing text.
//
// # Why hand-rolled
//
// The stdio transport is newline-delimited JSON-RPC 2.0 (MCP 2025-06-18,
// "Transports"), and the slice needed to serve three tools is `initialize`,
// `notifications/initialized`, `tools/list`, `tools/call` and `ping`. That is
// small and fully specified, and this module is deliberately dependency-free so
// the smallest possible surface carries its LGPL obligation. The same reasoning
// is already written down in the main module's internal/oci/blob.go, which
// hand-rolls the OCI JSON rather than pulling image-spec: "what raptormark
// emits is a narrow slice of the spec".
//
// # The one rule that matters
//
// The spec is emphatic: the server "MUST NOT write anything to its stdout that
// is not a valid MCP message", and messages "MUST NOT contain embedded
// newlines". Two consequences shape this file:
//
//   - Nothing here calls the decode package's os.Stdout-writing entry points.
//     It calls the library and renders into a buffer. A stray Println in a
//     report path would corrupt the stream in a way that looks like a client
//     bug.
//   - Responses are written with json.Marshal, never an Encoder with SetIndent.
//     Marshal escapes newlines inside strings, so one Marshal plus one "\n" is
//     exactly one message.
//
// Diagnostics go to stderr, which the spec explicitly allows.
package mcp

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"raptormark/tools/decode-oracle/internal/decode"
)

// protocolVersion is the revision this server implements.
//
// Version negotiation per the lifecycle spec: echo the client's version when we
// support it, otherwise reply with ours and let the client decide whether to
// continue. We do NOT fail the handshake on an unknown version -- the spec says
// to respond with a version we support, and an agent on an older revision can
// still call these tools.
const protocolVersion = "2025-06-18"

const serverName = "raptormark-decode-oracle"

// JSON-RPC 2.0 error codes used here.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// isNotification reports whether no response may be sent. A JSON-RPC
// notification is precisely a request with no id.
func (r *request) isNotification() bool { return len(r.ID) == 0 }

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// content is one item of an unstructured tool result.
type content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolResult struct {
	Content []content `json:"content"`
	// Structured mirrors Content as JSON where a tool has a natural shape.
	Structured any `json:"structuredContent,omitempty"`
	// IsError marks a TOOL EXECUTION failure, which the spec distinguishes from
	// a protocol error: a bad path or an unparseable encoding is the model's
	// problem to recover from and belongs here, while an unknown tool name is a
	// protocol error.
	IsError bool `json:"isError,omitempty"`
}

func textResult(s string, structured any) *toolResult {
	return &toolResult{Content: []content{{Type: "text", Text: s}}, Structured: structured}
}

func errResult(format string, args ...any) *toolResult {
	return &toolResult{
		Content: []content{{Type: "text", Text: fmt.Sprintf(format, args...)}},
		IsError: true,
	}
}

// tool is one exposed capability.
type tool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`

	call func(*Server, json.RawMessage) *toolResult `json:"-"`
}

// Server serves one stdio session.
type Server struct {
	In  io.Reader
	Out io.Writer
	Log io.Writer

	// Objdump is the cross-check disassembler passed to the decode package.
	Objdump string

	dec   *decode.Decoder
	tools []tool
}

// New builds a server over the given streams.
//
// The decode tables are parsed and validated once, here, rather than per call:
// a table that fails its own invariants would answer every tool call
// confidently and wrongly, so the failure belongs at startup where the client
// sees it as a launch failure.
func New(in io.Reader, out, logw io.Writer, objdump string) (*Server, error) {
	dec, err := decode.AArch64()
	if err != nil {
		return nil, fmt.Errorf("parsing the vendored decode tables: %w", err)
	}
	if probs := dec.Validate(); len(probs) != 0 {
		return nil, fmt.Errorf("a vendored decode table failed validation (%d problems); "+
			"re-vendor or fix the parser before serving", len(probs))
	}
	if objdump == "" {
		objdump = "objdump"
	}
	s := &Server{In: in, Out: out, Log: logw, Objdump: objdump, dec: dec}
	s.tools = builtinTools()
	return s, nil
}

// Serve runs the session until stdin closes.
func (s *Server) Serve() error {
	sc := bufio.NewScanner(s.In)
	// Corpus results and worklists are large, and a truncated line would be
	// silently invalid JSON rather than an obvious error.
	sc.Buffer(make([]byte, 0, 256*1024), 16*1024*1024)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			// No id is recoverable from an unparseable message, so the spec's
			// null-id error response is the only correct reply.
			s.write(&response{JSONRPC: "2.0", ID: json.RawMessage("null"),
				Error: &rpcError{Code: codeParseError, Message: "invalid JSON"}})
			continue
		}
		s.dispatch(&req)
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func (s *Server) dispatch(req *request) {
	if req.JSONRPC != "2.0" {
		if !req.isNotification() {
			s.fail(req, codeInvalidRequest, `"jsonrpc" must be "2.0"`, nil)
		}
		return
	}

	switch req.Method {
	case "initialize":
		s.reply(req, s.initialize(req.Params))

	case "notifications/initialized", "notifications/cancelled":
		// Notifications take no response, by definition.

	case "ping":
		// Utilities/ping: an empty result is the whole contract.
		s.reply(req, map[string]any{})

	case "tools/list":
		s.reply(req, map[string]any{"tools": s.tools})

	case "tools/call":
		s.callTool(req)

	default:
		if req.isNotification() {
			// Unknown notifications are ignored rather than answered; there is
			// nowhere to send an error and the spec allows forward compatibility.
			s.logf("ignoring unknown notification %q", req.Method)
			return
		}
		s.fail(req, codeMethodNotFound, "unknown method: "+req.Method, nil)
	}
}

func (s *Server) initialize(params json.RawMessage) map[string]any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)

	// "If the server supports the requested protocol version, it MUST respond
	// with the same version. Otherwise [...] respond with another protocol
	// version it supports."
	agreed := protocolVersion
	if p.ProtocolVersion == protocolVersion {
		agreed = p.ProtocolVersion
	} else if p.ProtocolVersion != "" {
		s.logf("client requested protocol %q; answering with %q", p.ProtocolVersion, protocolVersion)
	}

	return map[string]any{
		"protocolVersion": agreed,
		// listChanged is false: the tool set is fixed at compile time, so
		// promising notifications we will never send would be a lie the client
		// could wait on.
		"capabilities": map[string]any{
			"tools": map[string]any{"listChanged": false},
		},
		"serverInfo": map[string]any{
			"name":    serverName,
			"title":   "raptormark decode oracle",
			"version": version,
		},
		"instructions": instructions,
	}
}

func (s *Server) callTool(req *request) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		s.fail(req, codeInvalidParams, "invalid tools/call params", nil)
		return
	}
	for i := range s.tools {
		if s.tools[i].Name == p.Name {
			s.reply(req, s.tools[i].call(s, p.Arguments))
			return
		}
	}
	// An unknown tool is a protocol error, not a tool execution error.
	s.fail(req, codeInvalidParams, "unknown tool: "+p.Name, nil)
}

func (s *Server) reply(req *request, result any) {
	if req.isNotification() {
		return
	}
	s.write(&response{JSONRPC: "2.0", ID: req.ID, Result: result})
}

func (s *Server) fail(req *request, code int, msg string, data any) {
	id := req.ID
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	s.write(&response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg, Data: data}})
}

// write emits exactly one newline-delimited message.
func (s *Server) write(r *response) {
	b, err := json.Marshal(r)
	if err != nil {
		// Marshalling our own reply failed, so the reply cannot be sent. Say so
		// on stderr rather than emitting a partial line onto stdout.
		s.logf("marshalling response: %v", err)
		return
	}
	b = append(b, '\n')
	if _, err := s.Out.Write(b); err != nil {
		s.logf("writing response: %v", err)
	}
}

func (s *Server) logf(format string, args ...any) {
	if s.Log == nil {
		return
	}
	fmt.Fprintf(s.Log, serverName+": "+format+"\n", args...)
}
