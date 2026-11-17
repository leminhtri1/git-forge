package main

// A small HTTP/1.1 server on a raw net.Listener: keep-alive and pipelined requests, Content-Length and
// chunked request bodies, Expect: 100-continue, TCP_NODELAY, and every response written as one buffer in
// one Write. Requests on a connection are handled in order; handlers run under one global lock, so the
// store sees one request at a time exactly as the Node server's single thread did.

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
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

// Request is one parsed request; the body is read whole before the handler runs.
type Request struct {
	Method  string
	URL     string
	Path    string
	RawQ    string
	Headers map[string]string // lowercase names; repeats joined with ", "
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

func (r *Request) Header(name string) string { return r.Headers[name] }

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

var requestCounter uint64
var requestMu sync.Mutex

// nextRequestID is a fresh, non-empty request id (hex counter, GitHub-like shape).
func nextRequestID() string {
	requestMu.Lock()
	requestCounter++
	n := requestCounter
	requestMu.Unlock()
	return fmt.Sprintf("F0RG:%08X", n)
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

// isAuthorized: Bearer/token <token>, or Basic whose password is the token (a git credential helper).
func isAuthorized(r *Request) bool {
	header := strings.TrimSpace(r.Headers["authorization"])
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
	if config.Token == "" {
		return presented != ""
	}
	return presented == config.Token
}

func originOf(r *Request) string {
	host := r.Headers["host"]
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
	headers        map[string]string
	keepAlive      bool
	bodyStart      int
	length         int // -1: chunked
}

func serveConn(conn net.Conn, handler Handler) {
	defer conn.Close()
	buf := make([]byte, 0, 16*1024)
	tmp := make([]byte, 64*1024)
	read := func() bool {
		n, err := conn.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		return err == nil || n > 0
	}
	for {
		// the head
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
		// the body
		continued := false
		maybeContinue := func() {
			if !continued && strings.EqualFold(h.headers["expect"], "100-continue") {
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
			body = buf[h.bodyStart : h.bodyStart+h.length]
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
		req := &Request{Method: h.method, URL: h.target, Path: path, RawQ: rawQ, Headers: h.headers, Body: append([]byte(nil), body...)}
		resp := safeHandle(handler, req)
		out := serialize(resp, h.method == "HEAD", h.keepAlive)
		if _, err := conn.Write(out); err != nil {
			return
		}
		// keep what follows (a pipelined request), moved to the front of the buffer
		rest := len(buf) - consumed
		copy(buf, buf[consumed:])
		buf = buf[:rest]
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

func serialize(resp *Response, isHead, keepAlive bool) []byte {
	var b bytes.Buffer
	reason := reasons[resp.Status]
	if reason == "" {
		reason = "Unknown"
	}
	b.Grow(256 + len(resp.Body))
	b.WriteString("HTTP/1.1 ")
	b.WriteString(strconv.Itoa(resp.Status))
	b.WriteByte(' ')
	b.WriteString(reason)
	b.WriteString("\r\n")
	sawLength := false
	for _, kv := range resp.Headers {
		if strings.EqualFold(kv[0], "content-length") {
			sawLength = true
		}
		b.WriteString(kv[0])
		b.WriteString(": ")
		b.WriteString(kv[1])
		b.WriteString("\r\n")
	}
	noBody := resp.Status == 204 || resp.Status == 304
	if !sawLength && !noBody {
		b.WriteString("content-length: ")
		b.WriteString(strconv.Itoa(len(resp.Body)))
		b.WriteString("\r\n")
	}
	if !keepAlive {
		b.WriteString("connection: close\r\n")
	}
	b.WriteString("\r\n")
	if !isHead && !noBody {
		b.Write(resp.Body)
	}
	return b.Bytes()
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
	h := head{method: line[:sp1], target: line[sp1+1 : sp2], headers: make(map[string]string, 12), bodyStart: end + 4}
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
		name := strings.ToLower(strings.TrimSpace(l[:colon]))
		value := strings.TrimSpace(l[colon+1:])
		if prior, ok := h.headers[name]; ok {
			h.headers[name] = prior + ", " + value
		} else {
			h.headers[name] = value
		}
		at = next + 2
	}
	connection := strings.ToLower(h.headers["connection"])
	if version == "HTTP/1.1" {
		h.keepAlive = !strings.Contains(connection, "close")
	} else {
		h.keepAlive = strings.Contains(connection, "keep-alive")
	}
	if strings.Contains(strings.ToLower(h.headers["transfer-encoding"]), "chunked") {
		h.length = -1
	} else if cl, ok := h.headers["content-length"]; ok {
		if !digitsRE.MatchString(cl) || len(cl) > 15 {
			return head{}, false
		}
		h.length, _ = strconv.Atoi(cl)
	}
	return h, true
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
