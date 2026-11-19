package main

import (
	"bufio"
	"bytes"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type obj = map[string]any

const maxBody = 32 << 20

type request struct {
	method, path, query, authorization, gitProtocol, host string
	body                                                  []byte
}

type response []byte

func frame(status int, contentType, headers string, body []byte) response {
	out := make([]byte, 0, 110+len(contentType)+len(headers)+len(body))
	out = append(out, "HTTP/1.1 "...)
	out = strconv.AppendInt(out, int64(status), 10)
	out = append(out, ' ')
	out = append(out, http.StatusText(status)...)
	out = append(out, "\r\nContent-Length: "...)
	out = strconv.AppendInt(out, int64(len(body)), 10)
	if contentType != "" {
		out = append(out, "\r\nContent-Type: "...)
		out = append(out, contentType...)
	}
	out = append(out, "\r\nX-GitHub-Request-Id: forge\r\n"...)
	out = append(out, headers...)
	out = append(out, '\r', '\n')
	return append(out, body...)
}

func jsonFrame(status int, headers string, value any) response {
	body, err := json.Marshal(value)
	if err != nil {
		return frame(500, "application/json", "", []byte(`{"message":"Encoding error"}`))
	}
	return frame(status, "application/json", headers, body)
}

func problem(status int, message string) response {
	return jsonFrame(status, "", obj{"message": message})
}

var (
	notFound     = problem(404, "Not Found")
	unauthorized = problem(401, "Bad credentials")
	badRequest   = problem(400, "Invalid request")
	noContent    = frame(204, "application/json", "", nil)
	health       = jsonFrame(200, "", obj{"ok": true})
)

func text(v any) string { s, _ := v.(string); return s }
func object(v any) obj  { o, _ := v.(map[string]any); return o }
func items(v any) []any { a, _ := v.([]any); return a }
func integer(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	case string:
		i, _ := strconv.Atoi(n)
		return i
	}
	return 0
}

func getBody(body []byte) (obj, bool) {
	if len(body) == 0 {
		return obj{}, true
	}
	var value obj
	err := json.Unmarshal(body, &value)
	return value, err == nil && value != nil
}

func (f *forge) authenticated(header string) bool {
	kind, token, ok := strings.Cut(header, " ")
	if !ok || f.token == "" {
		return false
	}
	if strings.EqualFold(kind, "Basic") {
		decoded, err := base64.StdEncoding.DecodeString(token)
		if err != nil {
			return false
		}
		_, token, ok = strings.Cut(string(decoded), ":")
	} else if !strings.EqualFold(kind, "Bearer") && !strings.EqualFold(kind, "token") {
		return false
	}
	return ok && subtle.ConstantTimeCompare([]byte(token), []byte(f.token)) == 1
}

func (f *forge) dispatch(q request) response {
	if q.method == "GET" && q.path == "/healthz" {
		return health
	}
	if !f.authenticated(q.authorization) {
		return unauthorized
	}
	switch {
	case q.path == "/api/graphql" && q.method == "POST":
		return f.graphql(q.body)
	case q.path == "/mcp" && q.method == "POST":
		return f.mcp(q.body)
	case q.path == "/api/v3" || strings.HasPrefix(q.path, "/api/v3/"):
		return f.rest(q)
	default:
		return f.git(q)
	}
}

// Each connection owns its input buffer. Persistent responses are immutable and may
// be written concurrently without a serializer or a shared socket lock.
func (f *forge) serve(conn *net.TCPConn) {
	defer conn.Close()
	_ = conn.SetNoDelay(true)
	r := bufio.NewReaderSize(conn, 8192)
	var scratch []byte
	for {
		q, length, chunked, expect, closeAfter, err := readRequest(r)
		if err != nil {
			if err != io.EOF {
				_, _ = conn.Write(badRequest)
			}
			return
		}
		if expect {
			if _, err = conn.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n")); err != nil {
				return
			}
		}
		if chunked {
			scratch, err = readChunks(r, scratch[:0])
		} else if length != 0 {
			if cap(scratch) < length {
				scratch = make([]byte, length)
			}
			scratch = scratch[:length]
			_, err = io.ReadFull(r, scratch)
		} else {
			scratch = scratch[:0]
		}
		if err != nil {
			_, _ = conn.Write(badRequest)
			return
		}
		q.body = scratch
		if _, err = conn.Write(f.dispatch(q)); err != nil || closeAfter {
			return
		}
		if cap(scratch) > 1<<20 {
			scratch = nil
		}
	}
}

var invalidWire = fmt.Errorf("invalid HTTP request")

func readRequest(r *bufio.Reader) (q request, size int, chunked, expect, closeAfter bool, err error) {
	line, err := r.ReadSlice('\n')
	if err != nil {
		return
	}
	method, tail, ok := strings.Cut(string(bytes.TrimSuffix(line, []byte("\r\n"))), " ")
	target, version, hasVersion := strings.Cut(tail, " ")
	if !ok || !hasVersion || (version != "HTTP/1.1" && version != "HTTP/1.0") || !strings.HasPrefix(target, "/") {
		err = invalidWire
		return
	}
	q.method = method
	q.path, q.query, _ = strings.Cut(target, "?")
	if strings.Contains(q.path, "%") {
		q.path, err = url.PathUnescape(q.path)
		if err != nil {
			return
		}
	}
	closeAfter = version == "HTTP/1.0"
	seenLength, seenTransfer := false, false
	for budget := 32 << 10; ; {
		line, err = r.ReadSlice('\n')
		budget -= len(line)
		if err != nil {
			return
		}
		if budget < 0 || !bytes.HasSuffix(line, []byte("\r\n")) {
			err = invalidWire
			return
		}
		if len(line) == 2 {
			break
		}
		key, val, found := bytes.Cut(line[:len(line)-2], []byte{':'})
		if !found || len(key) == 0 {
			err = invalidWire
			return
		}
		for _, c := range key {
			if c <= ' ' || c >= 127 {
				err = invalidWire
				return
			}
		}
		val = bytes.TrimSpace(val)
		switch strings.ToLower(string(key)) {
		case "authorization":
			q.authorization = string(val)
		case "git-protocol":
			q.gitProtocol = string(val)
		case "host":
			q.host = string(val)
		case "connection":
			for token := range strings.SplitSeq(string(val), ",") {
				if strings.EqualFold(strings.TrimSpace(token), "close") {
					closeAfter = true
				}
			}
		case "expect":
			expect = bytes.EqualFold(val, []byte("100-continue"))
		case "content-length":
			if seenLength || len(val) == 0 {
				err = invalidWire
				return
			}
			seenLength = true
			for _, c := range val {
				if c < '0' || c > '9' {
					err = invalidWire
					return
				}
				size = size*10 + int(c-'0')
				if size > maxBody {
					err = invalidWire
					return
				}
			}
		case "transfer-encoding":
			if seenTransfer || !bytes.EqualFold(val, []byte("chunked")) {
				err = invalidWire
				return
			}
			seenTransfer, chunked = true, true
		}
	}
	if seenLength && chunked {
		err = invalidWire
	}
	return
}

func readChunks(r *bufio.Reader, dst []byte) ([]byte, error) {
	for {
		line, err := r.ReadSlice('\n')
		if err != nil {
			return nil, err
		}
		if !bytes.HasSuffix(line, []byte("\r\n")) {
			return nil, invalidWire
		}
		size, _, _ := bytes.Cut(line[:len(line)-2], []byte{';'})
		n, err := strconv.ParseUint(string(size), 16, 32)
		if err != nil || n > uint64(maxBody-len(dst)) {
			return nil, invalidWire
		}
		if n == 0 {
			for budget := 8192; budget > 0; {
				trailer, err := r.ReadSlice('\n')
				if err != nil {
					return nil, err
				}
				if bytes.Equal(trailer, []byte("\r\n")) {
					return dst, nil
				}
				budget -= len(trailer)
			}
			return nil, invalidWire
		}
		start := len(dst)
		dst = append(dst, make([]byte, int(n))...)
		if _, err = io.ReadFull(r, dst[start:]); err != nil {
			return nil, err
		}
		var end [2]byte
		if _, err = io.ReadFull(r, end[:]); err != nil || end != [2]byte{'\r', '\n'} {
			return nil, invalidWire
		}
	}
}

func runTrial(origin string, port int) error {
	client := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	origin = strings.TrimRight(origin, "/")
	registered := false
	for attempt := 0; attempt < 30; attempt++ {
		body := strings.NewReader(fmt.Sprintf(`{"port":%d}`, port))
		res, err := client.Post(origin+"/register", "application/json", body)
		if err == nil {
			_, _ = io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode < 300 || res.StatusCode == 409 {
				registered = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !registered {
		return fmt.Errorf("forge port registration failed")
	}
	for {
		res, err := client.Get(origin + "/done")
		if err == nil {
			var state struct{ Done bool }
			err = json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&state)
			res.Body.Close()
			if err == nil && state.Done {
				return nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func main() {
	address := "0.0.0.0:0"
	if runtime.GOOS == "windows" || os.Getenv("MAKEFUN_ENV_URL") == "" {
		address = "127.0.0.1:0"
	}
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.ParseIP(strings.Split(address, ":")[0])})
	if err != nil {
		log.Fatal(err)
	}
	defer listener.Close()
	f := newForge()
	port := listener.Addr().(*net.TCPAddr).Port
	fmt.Printf("forge listening on %d\n", port)
	go func() {
		for {
			conn, err := listener.AcceptTCP()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	if origin := os.Getenv("MAKEFUN_ENV_URL"); origin != "" {
		if err := runTrial(origin, port); err != nil {
			log.Fatal(err)
		}
	} else {
		select {}
	}
}
