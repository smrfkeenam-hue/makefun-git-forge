// HTTP/1.1 over raw TCP: the transport the whole forge speaks. One goroutine
// per connection, requests parsed and answered sequentially in arrival order
// (pipelining included); keep-alive by default on 1.1, close by default on
// 1.0. Bodies are Content-Length or chunked; Expect: 100-continue is answered
// as soon as the head is parsed. A handler panic becomes a 500 with the
// socket left open; a malformed head or an over-limit body is a 400/413 and
// the socket closes.
//
// Every response carries X-GitHub-Request-Id: req-%08x (a process-wide atomic
// counter starting at 1), including errors. The response version echoes the
// request version (1.0 or 1.1). 204/304 responses carry no content-length;
// HEAD responses carry the content-length but no body bytes.

package main

import (
	"bytes"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
)

// HttpRequest is one parsed request: method, raw target, headers (lowercased
// names, first occurrence wins), and the fully-read body.
type HttpRequest struct {
	Method  string
	Target  string
	Version string // "1.0" or "1.1"
	Headers map[string]string
	Body    []byte
}

// HttpResponse is one reply: status, headers, body bytes.
type HttpResponse struct {
	Status  int
	Headers [][2]string
	Body    []byte
}

// Handler answers one request. It runs under a mutex in the app layer, so
// handlers see the same serialized semantics as the single-threaded Node
// server they port.
type Handler func(req *HttpRequest) HttpResponse

const (
	maxHeadBytes = 64 * 1024
	maxBodyBytes = 64 * 1024 * 1024
)

var httpRequestCounter int64 // atomic; the first request is req-00000001

func nextRequestID() string {
	n := atomic.AddInt64(&httpRequestCounter, 1)
	return fmt.Sprintf("req-%08x", n)
}

var reasonPhrases = map[int]string{
	200: "OK",
	201: "Created",
	204: "No Content",
	400: "Bad Request",
	401: "Unauthorized",
	404: "Not Found",
	405: "Method Not Allowed",
	409: "Conflict",
	413: "Content Too Large",
	422: "Unprocessable Entity",
	500: "Internal Server Error",
}

func reasonPhrase(status int) string {
	if phrase, ok := reasonPhrases[status]; ok {
		return phrase
	}
	return "OK"
}

// connState is one connection's read buffer plus its socket.
type connState struct {
	conn net.Conn
	buf  *bytes.Buffer
	tmp  []byte
}

func newConnState(conn net.Conn) *connState {
	return &connState{conn: conn, buf: &bytes.Buffer{}, tmp: make([]byte, 32768)}
}

// fill reads one chunk from the socket into the buffer. It returns false on
// error or EOF.
func (c *connState) fill() bool {
	n, err := c.conn.Read(c.tmp)
	if n > 0 {
		c.buf.Write(c.tmp[:n])
	}
	return err == nil
}

// readHead reads until the head terminator "\r\n\r\n" is in the buffer and
// returns the head bytes (without the terminator). ok=false means the
// connection ended or the head exceeded 64 KB (overLimit distinguishes the
// two).
func (c *connState) readHead() (head []byte, ok bool, overLimit bool) {
	for {
		if idx := bytes.Index(c.buf.Bytes(), []byte("\r\n\r\n")); idx >= 0 {
			head = append([]byte(nil), c.buf.Bytes()[:idx]...)
			c.buf.Next(idx + 4)
			return head, true, false
		}
		if c.buf.Len() > maxHeadBytes {
			return nil, false, true
		}
		if !c.fill() {
			return nil, false, false
		}
	}
}

// readExact reads exactly n bytes (from the buffer, refilling from the socket
// as needed). It returns false if the connection ends first.
func (c *connState) readExact(n int) ([]byte, bool) {
	for c.buf.Len() < n {
		if !c.fill() {
			return nil, false
		}
	}
	return append([]byte(nil), c.buf.Next(n)...), true
}

// readLine reads one "\r\n"-terminated line (without the terminator). It
// returns false if the connection ends first.
func (c *connState) readLine() (string, bool) {
	for {
		if idx := bytes.IndexByte(c.buf.Bytes(), '\n'); idx >= 0 {
			line := c.buf.Bytes()[:idx]
			c.buf.Next(idx + 1)
			return strings.TrimSuffix(string(line), "\r"), true
		}
		if !c.fill() {
			return "", false
		}
	}
}

// readChunkedBody reads a chunked body: size lines (hex, optional
// ";extension"), data, trailers until a blank line. ok=false means the
// connection ended; overLimit means the body passed 64 MB; badChunk means a
// chunk size did not parse.
func (c *connState) readChunkedBody() (body []byte, ok bool, overLimit bool, badChunk bool) {
	var out []byte
	for {
		line, ok := c.readLine()
		if !ok {
			return nil, false, false, false
		}
		sizeText := strings.TrimSpace(strings.SplitN(line, ";", 2)[0])
		size, err := strconv.ParseInt(sizeText, 16, 64)
		if err != nil || size < 0 {
			// Node's parseInt(hex) is NaN on a bad size, and a negative
			// size (from a "-1" line) is also a 400 there.
			return nil, false, false, true
		}
		if size == 0 {
			// Trailers until a blank line.
			for {
				trailer, ok := c.readLine()
				if !ok {
					return nil, false, false, false
				}
				if trailer == "" {
					return out, true, false, false
				}
			}
		}
		if int64(len(out))+size > maxBodyBytes {
			return nil, false, true, false
		}
		chunk, ok := c.readExact(int(size))
		if !ok {
			return nil, false, false, false
		}
		out = append(out, chunk...)
		// The trailing CRLF after each chunk.
		if _, ok := c.readExact(2); !ok {
			return nil, false, false, false
		}
	}
}

// writeResponse writes one response: head and body in a single writev (no
// copy of the body), with the request id and the keep-alive decision. HEAD
// requests get the content-length but no body bytes.
func writeResponse(conn net.Conn, version string, reqID string, keepAlive bool, resp HttpResponse, headOnly bool) bool {
	var head strings.Builder
	head.WriteString("HTTP/")
	head.WriteString(version)
	head.WriteString(" ")
	head.WriteString(strconv.Itoa(resp.Status))
	head.WriteString(" ")
	head.WriteString(reasonPhrase(resp.Status))
	head.WriteString("\r\n")
	for _, h := range resp.Headers {
		head.WriteString(h[0])
		head.WriteString(": ")
		head.WriteString(h[1])
		head.WriteString("\r\n")
	}
	if resp.Status != 204 && resp.Status != 304 {
		head.WriteString("content-length: ")
		head.WriteString(strconv.Itoa(len(resp.Body)))
		head.WriteString("\r\n")
	}
	head.WriteString("x-github-request-id: ")
	head.WriteString(reqID)
	head.WriteString("\r\n")
	head.WriteString("connection: ")
	if keepAlive {
		head.WriteString("keep-alive")
	} else {
		head.WriteString("close")
	}
	head.WriteString("\r\n\r\n")
	headBytes := []byte(head.String())
	buffers := net.Buffers{headBytes}
	if !headOnly && len(resp.Body) > 0 {
		buffers = append(buffers, resp.Body)
	}
	_, err := buffers.WriteTo(conn)
	return err == nil
}

// writeError writes a JSON error reply ({"message":...}\n) with a fresh
// request id and closes the socket afterwards (the caller closes).
func writeError(conn net.Conn, version string, status int, message string) {
	body := marshalJSON(map[string]any{"message": message})
	body = append(body, '\n')
	resp := HttpResponse{
		Status:  status,
		Headers: [][2]string{{"content-type", "application/json"}},
		Body:    body,
	}
	writeResponse(conn, version, nextRequestID(), false, resp, false)
}

// serveConn handles one connection until it closes or a request ends it.
func serveConn(conn net.Conn, handler Handler) {
	defer conn.Close()
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetNoDelay(true)
	}
	state := newConnState(conn)
	for {
		head, ok, overLimit := state.readHead()
		if !ok {
			if overLimit {
				writeError(conn, "1.1", 413, "Request Entity Too Large")
			}
			return
		}
		headText := string(head)
		// A head with only a request line and no headers is a 400.
		if !strings.Contains(headText, "\r\n") {
			writeError(conn, "1.1", 400, "Bad Request")
			return
		}
		lines := strings.Split(headText, "\r\n")
		requestLine := lines[0]
		parts := strings.Split(requestLine, " ")
		method := ""
		rawTarget := ""
		if len(parts) > 0 {
			method = parts[0]
		}
		if len(parts) > 1 {
			rawTarget = parts[1]
		}
		version := "1.1"
		if strings.HasSuffix(requestLine, " HTTP/1.0") {
			version = "1.0"
		}
		headers := map[string]string{}
		for _, line := range lines[1:] {
			colon := strings.Index(line, ":")
			if colon == -1 {
				continue
			}
			name := strings.ToLower(strings.TrimSpace(line[:colon]))
			if _, seen := headers[name]; seen {
				continue // first occurrence wins
			}
			headers[name] = strings.TrimSpace(line[colon+1:])
		}
		keepAlive := true
		if version == "1.1" {
			if strings.ToLower(headers["connection"]) == "close" {
				keepAlive = false
			}
		} else {
			keepAlive = strings.ToLower(headers["connection"]) == "keep-alive"
		}
		// 100-continue is answered as soon as the head is parsed, before the
		// body is read.
		if strings.Contains(strings.ToLower(headers["expect"]), "100-continue") {
			_, _ = conn.Write([]byte("HTTP/" + version + " 100 Continue\r\n\r\n"))
		}
		// Body framing: chunked wins over content-length.
		var body []byte
		if strings.Contains(strings.ToLower(headers["transfer-encoding"]), "chunked") {
			chunked, ok, overLimit, badChunk := state.readChunkedBody()
			if !ok {
				if overLimit {
					writeError(conn, version, 413, "Request Entity Too Large")
				} else if badChunk {
					writeError(conn, version, 400, "Bad Request")
				}
				return
			}
			body = chunked
		} else if cl, present := headers["content-length"]; present {
			length, err := strconv.Atoi(strings.TrimSpace(cl))
			if err != nil || length <= 0 {
				length = 0
			}
			if length > maxBodyBytes {
				writeError(conn, version, 413, "Request Entity Too Large")
				return
			}
			if length > 0 {
				read, ok := state.readExact(length)
				if !ok {
					return
				}
				body = read
			}
		}
		req := &HttpRequest{
			Method:  method,
			Target:  rawTarget,
			Version: version,
			Headers: headers,
			Body:    body,
		}
		resp := dispatchSafe(handler, req)
		if !writeResponse(conn, version, nextRequestID(), keepAlive, resp, method == "HEAD") {
			return
		}
		if !keepAlive {
			return
		}
	}
}

// dispatchSafe runs the handler, turning a panic into a 500 with the socket
// left open (the Node server's dispatch catch does the same).
func dispatchSafe(handler Handler, req *HttpRequest) (resp HttpResponse) {
	defer func() {
		if recover() != nil {
			body := marshalJSON(map[string]any{"message": "Internal Server Error"})
			body = append(body, '\n')
			resp = HttpResponse{
				Status:  500,
				Headers: [][2]string{{"content-type", "application/json"}},
				Body:    body,
			}
		}
	}()
	return handler(req)
}

// Serve accepts connections on the listener and serves them until it closes.
func Serve(listener net.Listener, handler Handler) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go serveConn(conn, handler)
	}
}
