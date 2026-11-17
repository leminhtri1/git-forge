package main

// A small HTTP/1.1 server on a raw net.Listener: keep-alive and pipelined requests, Content-Length and
// chunked request bodies, Expect: 100-continue, TCP_NODELAY, and every response written as one buffer in
// one Write. Requests on a connection are handled in order; handlers run under one global lock, so the
// store sees one request at a time exactly as the Node server's single thread did.
//
// Per request the hot path allocates little: the head is parsed into a header slice (no map), the body
// is a view of the connection's read buffer (handlers copy whatever they keep), and the response is
// serialised into the connection's own reusable output buffer.

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	maxHead = 64 * 1024
	maxBody = 512 * 1024 * 1024
)

var crlfcrlf = []byte("\r\n\r\n")

var reasons = map[int]string{
	200: "OK", 201: "Created", 202: "Accepted", 204: "No Content", 301: "Moved Permanently", 302: "Found", 304: "Not Modified",
	400: "Bad Request", 401: "Unauthorized", 403: "Forbidden", 404: "Not Found", 405: "Method Not Allowed", 409: "Conflict",
	413: "Payload Too Large", 415: "Unsupported Media Type", 422: "Unprocessable Entity", 431: "Request Header Fields Too Large",
	500: "Internal Server Error", 501: "Not Implemented", 503: "Service Unavailable",
}

// Headers holds a request's headers: lowercase names; repeats joined with ", ".
type Headers []struct{ name, value string }

func (h Headers) Get(name string) string {
	for i := range h {
		if h[i].name == name {
			return h[i].value
		}
	}
	return ""
}

func (h *Headers) add(name, value string) {
	for i := range *h {
		if (*h)[i].name == name {
			(*h)[i].value += ", " + value
			return
		}
	}
	*h = append(*h, struct{ name, value string }{name, value})
}

// Request is one parsed request; the body is read whole before the handler runs. Body is only valid
// while the handler runs (it is a view of the connection's buffer).
type Request struct {
	Method  string
	URL     string
	Path    string
	RawQ    string
	Headers Headers
	Body    []byte
	query   url.Values
}

func (r *Request) Query() url.Values {
	if r.query == nil {
		r.query, _ = url.ParseQuery(r.RawQ)
		if r.query == nil {
			r.query = url.Values{}
		}
	}
	return r.query
}

func (r *Request) Header(name string) string { return r.Headers.Get(name) }

// Response is what a handler answers; the server serialises it.
type Response struct {
	Status  int
	Headers [][2]string
	Body    []byte
}

func (r *Response) Add(name, value string) *Response {
	r.Headers = append(r.Headers, [2]string{name, value})
	return r
}

const jsonType = "application/json; charset=utf-8"

var requestCounter atomic.Uint64

const upperHex = "0123456789ABCDEF"

// nextRequestID is a fresh, non-empty request id (hex counter, GitHub-like shape: F0RG:0000002A).
func nextRequestID() string {
	n := requestCounter.Add(1)
	var b [13]byte
	copy(b[:], "F0RG:")
	for i := 12; i >= 5; i-- {
		b[i] = upperHex[n&0xf]
		n >>= 4
	}
	if n == 0 {
		return string(b[:])
	}
	return fmt.Sprintf("F0RG:%08X", requestCounter.Load())
}

func jsonBytesResp(status int, body []byte) *Response {
	return &Response{Status: status, Headers: [][2]string{{"content-type", jsonType}, {"x-github-request-id", nextRequestID()}}, Body: body}
}

func jsonResp(status int, v any) *Response {
	return jsonBytesResp(status, toJSON(v))
}

// HTTPError is a handler's failure answered as GitHub's JSON error body.
type HTTPError struct {
	Status  int
	Message string
	Extra   Obj
}

func (e *HTTPError) Error() string { return e.Message }

func httpErr(status int, message string) *HTTPError {
	return &HTTPError{Status: status, Message: message}
}

func errorResp(status int, message string, extra Obj) *Response {
	body := Obj{{"message", message}, {"documentation_url", "https://docs.github.com/rest"}}
	body = append(body, extra...)
	return jsonResp(status, body)
}

func emptyResp(status int) *Response {
	return &Response{Status: status, Headers: [][2]string{{"x-github-request-id", nextRequestID()}}}
}

var lastAuthHeader string // the last header that matched the token: most requests repeat it verbatim

// isAuthorized: Bearer/token <token>, or Basic whose password is the token (a git credential helper).
func isAuthorized(r *Request) bool {
	raw := r.Headers.Get("authorization")
	if raw != "" && raw == lastAuthHeader {
		return true
	}
	header := strings.TrimSpace(raw)
	space := strings.IndexByte(header, ' ')
	if space < 0 {
		return false
	}
	scheme := strings.ToLower(header[:space])
	value := strings.TrimSpace(header[space+1:])
	if value == "" {
		return false
	}
	presented := value
	switch scheme {
	case "basic":
		decoded, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return false
		}
		s := string(decoded)
		if colon := strings.IndexByte(s, ':'); colon >= 0 {
			presented = s[colon+1:]
		} else {
			presented = s
		}
	case "bearer", "token":
	default:
		return false
	}
	ok := presented != ""
	if config.Token != "" {
		ok = presented == config.Token
	}
	if ok {
		lastAuthHeader = raw
	}
	return ok
}

func originOf(r *Request) string {
	host := r.Headers.Get("host")
	if host == "" {
		host = "localhost"
	}
	return "http://" + host
}

// ---------------------------------------------------------------- the server

type Handler func(*Request) *Response

var handleMu sync.Mutex

func serve(ln net.Listener, handler Handler) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.SetNoDelay(true)
		}
		go serveConn(conn, handler)
	}
}

type head struct {
	method, target string
	headers        Headers
	keepAlive      bool
	bodyStart      int
	length         int // -1: chunked
}

const readChunk = 64 * 1024

func serveConn(conn net.Conn, handler Handler) {
	defer conn.Close()
	buf := make([]byte, 0, readChunk)
	out := make([]byte, 0, 16*1024)
	// read appends straight into buf's spare capacity: one syscall reads up to 64 KiB.
	read := func() bool {
		if cap(buf)-len(buf) < readChunk/2 {
			grown := make([]byte, len(buf), 2*cap(buf)+readChunk)
			copy(grown, buf)
			buf = grown
		}
		n, err := conn.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		return err == nil || n > 0
	}
	for {
		end := bytes.Index(buf, crlfcrlf)
		for end < 0 {
			if len(buf) > maxHead {
				writeFail(conn, 431)
				return
			}
			if !read() {
				return
			}
			end = bytes.Index(buf, crlfcrlf)
		}
		h, ok := parseHead(buf, end)
		if !ok {
			writeFail(conn, 400)
			return
		}
		if h.length > maxBody {
			writeFail(conn, 413)
			return
		}
		continued := false
		maybeContinue := func() {
			if !continued && strings.EqualFold(h.headers.Get("expect"), "100-continue") {
				continued = true
				_, _ = conn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n"))
			}
		}
		var body []byte
		consumed := 0
		if h.length >= 0 {
			for len(buf) < h.bodyStart+h.length {
				maybeContinue()
				if !read() {
					return
				}
			}
			body = buf[h.bodyStart : h.bodyStart+h.length : h.bodyStart+h.length]
			consumed = h.bodyStart + h.length
		} else {
			for {
				decoded, used, state := decodeChunked(buf, h.bodyStart)
				if state == chunkBad {
					writeFail(conn, 400)
					return
				}
				if state == chunkDone {
					body = decoded
					consumed = used
					break
				}
				maybeContinue()
				if !read() {
					return
				}
			}
		}
		path, rawQ := h.target, ""
		if q := strings.IndexByte(h.target, '?'); q >= 0 {
			path, rawQ = h.target[:q], h.target[q+1:]
		}
		req := &Request{Method: h.method, URL: h.target, Path: path, RawQ: rawQ, Headers: h.headers, Body: body}
		resp := safeHandle(handler, req)
		out = serialize(out[:0], resp, h.method == "HEAD", h.keepAlive)
		if _, err := conn.Write(out); err != nil {
			return
		}
		if cap(out) > 4*1024*1024 {
			out = make([]byte, 0, 16*1024) // do not pin a large clone pack per idle connection
		}
		// keep what follows (a pipelined request), moved to the front of the buffer
		rest := copy(buf, buf[consumed:])
		buf = buf[:rest]
		if cap(buf) > 4*1024*1024 && rest < readChunk {
			small := make([]byte, rest, readChunk)
			copy(small, buf)
			buf = small
		}
		if !h.keepAlive {
			return
		}
	}
}

func safeHandle(handler Handler, req *Request) (resp *Response) {
	defer func() {
		if r := recover(); r != nil {
			if he, ok := r.(*HTTPError); ok {
				resp = errorResp(he.Status, he.Message, he.Extra)
				return
			}
			fmt.Printf("[forge] %s %s: panic %v\n", req.Method, req.URL, r)
			resp = errorResp(500, "Internal Server Error", nil)
		}
	}()
	handleMu.Lock()
	defer handleMu.Unlock()
	return handler(req)
}

// serialize appends status line, headers and body to out.
func serialize(out []byte, resp *Response, isHead, keepAlive bool) []byte {
	reason := reasons[resp.Status]
	if reason == "" {
		reason = "Unknown"
	}
	out = append(out, "HTTP/1.1 "...)
	out = strconv.AppendInt(out, int64(resp.Status), 10)
	out = append(out, ' ')
	out = append(out, reason...)
	out = append(out, "\r\n"...)
	sawLength := false
	for _, kv := range resp.Headers {
		if len(kv[0]) == 14 && strings.EqualFold(kv[0], "content-length") {
			sawLength = true
		}
		out = append(out, kv[0]...)
		out = append(out, ": "...)
		out = append(out, kv[1]...)
		out = append(out, "\r\n"...)
	}
	noBody := resp.Status == 204 || resp.Status == 304
	if !sawLength && !noBody {
		out = append(out, "content-length: "...)
		out = strconv.AppendInt(out, int64(len(resp.Body)), 10)
		out = append(out, "\r\n"...)
	}
	if !keepAlive {
		out = append(out, "connection: close\r\n"...)
	}
	out = append(out, "\r\n"...)
	if !isHead && !noBody {
		out = append(out, resp.Body...)
	}
	return out
}

func writeFail(conn net.Conn, status int) {
	body := fmt.Sprintf(`{"message":%q}`, reasons[status])
	_, _ = fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\ncontent-type: %s\r\ncontent-length: %d\r\nconnection: close\r\n\r\n%s", status, reasons[status], jsonType, len(body), body)
}

func parseHead(buf []byte, end int) (head, bool) {
	text := string(buf[:end])
	lineEnd := strings.Index(text, "\r\n")
	if lineEnd < 0 {
		lineEnd = len(text)
	}
	line := text[:lineEnd]
	sp1 := strings.IndexByte(line, ' ')
	sp2 := strings.LastIndexByte(line, ' ')
	if sp1 <= 0 || sp2 <= sp1 {
		return head{}, false
	}
	h := head{method: line[:sp1], target: line[sp1+1 : sp2], headers: make(Headers, 0, 12), bodyStart: end + 4}
	version := line[sp2+1:]
	if version != "HTTP/1.1" && version != "HTTP/1.0" {
		return head{}, false
	}
	at := lineEnd + 2
	for at < len(text) {
		next := strings.Index(text[at:], "\r\n")
		if next < 0 {
			next = len(text)
		} else {
			next += at
		}
		l := text[at:next]
		colon := strings.IndexByte(l, ':')
		if colon <= 0 {
			return head{}, false
		}
		h.headers.add(lowerASCII(strings.TrimSpace(l[:colon])), strings.TrimSpace(l[colon+1:]))
		at = next + 2
	}
	connection := lowerASCII(h.headers.Get("connection"))
	if version == "HTTP/1.1" {
		h.keepAlive = !strings.Contains(connection, "close")
	} else {
		h.keepAlive = strings.Contains(connection, "keep-alive")
	}
	if te := h.headers.Get("transfer-encoding"); te != "" && strings.Contains(lowerASCII(te), "chunked") {
		h.length = -1
	} else if cl := h.headers.Get("content-length"); cl != "" {
		if !digitsRE.MatchString(cl) || len(cl) > 15 {
			return head{}, false
		}
		h.length, _ = strconv.Atoi(cl)
	}
	return h, true
}

// lowerASCII lowercases without allocating when the text is already lowercase.
func lowerASCII(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			return strings.ToLower(s)
		}
	}
	return s
}

const (
	chunkMore = iota
	chunkDone
	chunkBad
)

func decodeChunked(buf []byte, start int) ([]byte, int, int) {
	var out []byte
	at := start
	for {
		lineEnd := bytes.Index(buf[at:], []byte("\r\n"))
		if lineEnd < 0 {
			return nil, 0, chunkMore
		}
		sizeText := string(buf[at : at+lineEnd])
		if semi := strings.IndexByte(sizeText, ';'); semi >= 0 {
			sizeText = sizeText[:semi]
		}
		size, err := strconv.ParseInt(strings.TrimSpace(sizeText), 16, 64)
		if err != nil || size < 0 {
			return nil, 0, chunkBad
		}
		at += lineEnd + 2
		if size == 0 {
			for {
				trailerEnd := bytes.Index(buf[at:], []byte("\r\n"))
				if trailerEnd < 0 {
					return nil, 0, chunkMore
				}
				at += trailerEnd + 2
				if trailerEnd == 0 {
					if out == nil {
						out = []byte{}
					}
					return out, at, chunkDone
				}
			}
		}
		if at+int(size)+2 > len(buf) {
			return nil, 0, chunkMore
		}
		out = append(out, buf[at:at+int(size)]...)
		if len(out) > maxBody {
			return nil, 0, chunkBad
		}
		at += int(size) + 2
	}
}
