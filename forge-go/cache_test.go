package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func apiCall(t *testing.T, f *forge, method, path, body string, status int) []byte {
	t.Helper()
	result := f.dispatch(request{method: method, path: "/api/v3" + path, host: "forge.local", authorization: "Bearer test-token", body: []byte(body)})
	res, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(result)), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != status {
		t.Fatalf("%s %s: wanted %d, got %s", method, path, status, result)
	}
	_, payload, _ := bytes.Cut(result, []byte("\r\n\r\n"))
	return payload
}

func assertListSize(t *testing.T, f *forge, path string, count int) {
	t.Helper()
	var list []obj
	if err := json.Unmarshal(apiCall(t, f, "GET", path, "", 200), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != count {
		t.Fatalf("%s: wanted %d objects, got %v", path, count, list)
	}
}

func TestCachedListsFollowMutations(t *testing.T) {
	t.Setenv("MAKEFUN_SHOP_TOKEN", "test-token")
	t.Setenv("MAKEFUN_FORGE_LOGIN", "adaiversen")
	f := newForge()
	apiCall(t, f, "POST", "/user/repos", `{"name":"cache","auto_init":true}`, 201)
	path := "/repos/adaiversen/cache"
	assertListSize(t, f, path+"/issues", 0)
	apiCall(t, f, "POST", path+"/issues", `{"title":"First"}`, 201)
	assertListSize(t, f, path+"/issues", 1)
	apiCall(t, f, "PATCH", path+"/issues/1", `{"title":"Updated"}`, 200)
	if body := apiCall(t, f, "GET", path+"/issues", "", 200); !strings.Contains(string(body), `"title":"Updated"`) {
		t.Fatalf("cached title is stale: %s", body)
	}
	apiCall(t, f, "POST", path+"/issues/1/comments", `{"body":"Comment"}`, 201)
	if body := apiCall(t, f, "GET", path+"/issues", "", 200); !strings.Contains(string(body), `"comments":1`) {
		t.Fatalf("cached comment count is stale: %s", body)
	}
	apiCall(t, f, "PATCH", path+"/issues/1", `{"state":"closed"}`, 200)
	assertListSize(t, f, path+"/issues", 0)

	r := f.repo("adaiversen", "cache")
	apiCall(t, f, "POST", path+"/git/refs", `{"ref":"refs/heads/topic","sha":"`+r.resolve("main").String()+`"}`, 201)
	apiCall(t, f, "PUT", path+"/contents/topic.txt", `{"branch":"topic","content":"aGVsbG8=","message":"Topic"}`, 201)
	assertListSize(t, f, path+"/pulls", 0)
	apiCall(t, f, "POST", path+"/pulls", `{"title":"Merge topic","head":"topic","base":"main"}`, 201)
	assertListSize(t, f, path+"/pulls", 1)
	advertise := request{method: "GET", path: "/adaiversen/cache.git/info/refs", query: "service=git-receive-pack", authorization: "Bearer test-token"}
	before := f.dispatch(advertise)
	if repeated := f.dispatch(advertise); &repeated[0] != &before[0] {
		t.Fatal("info/refs did not retain its complete HTTP frame")
	}
	refsBody := append(packet(nil, "command=ls-refs\n"), delim...)
	refsBody = append(packet(refsBody, "ref-prefix refs/heads/main\n"), flush...)
	refs := request{method: "POST", path: "/adaiversen/cache.git/git-upload-pack", gitProtocol: "version=2", authorization: "Bearer test-token", body: refsBody}
	refsBefore := f.dispatch(refs)
	if repeated := f.dispatch(refs); !bytes.Equal(refsBefore, repeated) || &refsBefore[0] != &repeated[0] {
		t.Fatal("repeated ls-refs changed without a mutation")
	}
	apiCall(t, f, "PUT", path+"/pulls/2/merge", `{}`, 200)
	assertListSize(t, f, path+"/pulls", 0)
	after := f.dispatch(advertise)
	if bytes.Equal(before, after) || !bytes.Contains(after, []byte(r.resolve("main").String()+" refs/heads/main")) {
		t.Fatal("merge did not refresh the advertised branch tip")
	}
	if after := f.dispatch(refs); bytes.Equal(refsBefore, after) || !bytes.Contains(after, []byte(r.resolve("main").String()+" refs/heads/main")) {
		t.Fatal("merge did not refresh the cached ls-refs reply")
	}
}

func TestIssueListCacheReusesBytesAndInvalidatesAtomically(t *testing.T) {
	t.Setenv("MAKEFUN_SHOP_TOKEN", "test-token")
	t.Setenv("MAKEFUN_FORGE_LOGIN", "adaiversen")
	f := newForge()
	apiCall(t, f, "POST", "/user/repos", `{"name":"bounded"}`, 201)
	path := "/repos/adaiversen/bounded/issues"
	assertListSize(t, f, path, 0)
	r := f.repo("adaiversen", "bounded")
	cached := r.wire.Load()
	assertListSize(t, f, path, 0)
	if cached == nil || cached != r.wire.Load() {
		t.Fatal("identical list reads did not reuse immutable bytes")
	}
	apiCall(t, f, "POST", path, `{"title":"New"}`, 201)
	if r.wire.Load() != nil {
		t.Fatal("mutation did not atomically invalidate the cached list")
	}
	for _, value := range cached.entries {
		if !bytes.HasSuffix(value, []byte("[]")) {
			t.Fatal("mutation modified an already published response")
		}
	}
	assertListSize(t, f, path, 1)
}

func TestWireCacheLimitsAndConcurrentPublish(t *testing.T) {
	r := &repository{}
	var workers sync.WaitGroup
	for i := 0; i < 300; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			r.mu.RLock()
			defer r.mu.RUnlock()
			r.storeWire(strconv.Itoa(i), health)
		}(i)
	}
	workers.Wait()
	frames := r.wire.Load()
	if len(frames.entries) != maxWireEntries || frames.bytes > maxWireBytes {
		t.Fatalf("cache exceeded limits: %d entries, %d bytes", len(frames.entries), frames.bytes)
	}
	r.mu.Lock()
	r.invalidateWire()
	for i := 0; i < 300; i++ {
		r.storeWire(strconv.Itoa(i), frame(200, "application/json", "", bytes.Repeat([]byte{'x'}, 32000)))
	}
	r.mu.Unlock()
	frames = r.wire.Load()
	if frames.bytes > maxWireBytes || len(frames.entries) >= maxWireEntries {
		t.Fatalf("byte budget not enforced: %d entries, %d bytes", len(frames.entries), frames.bytes)
	}
	r.mu.RLock()
	r.storeWire("oversized", make(response, maxWireEntry+1))
	r.mu.RUnlock()
	if r.cachedWire("oversized") != nil {
		t.Fatal("oversized frame was retained")
	}
}

func TestWirePublicationCannotResurrectBeforeMutation(t *testing.T) {
	r := &repository{}
	r.mu.RLock()
	started, done := make(chan struct{}), make(chan struct{})
	go func() {
		close(started)
		r.mu.Lock()
		r.invalidateWire()
		r.mu.Unlock()
		close(done)
	}()
	<-started
	r.storeWire("before", health)
	r.mu.RUnlock()
	<-done
	if r.wire.Load() != nil {
		t.Fatal("a stale publisher survived the repository mutation")
	}
}

func TestWireCacheRetainsQueryVariantsAndCompleteFrames(t *testing.T) {
	t.Setenv("MAKEFUN_SHOP_TOKEN", "test-token")
	t.Setenv("MAKEFUN_FORGE_LOGIN", "adaiversen")
	f := newForge()
	apiCall(t, f, "POST", "/user/repos", `{"name":"variants"}`, 201)
	path := "/repos/adaiversen/variants/issues"
	apiCall(t, f, "POST", path, `{"title":"One"}`, 201)
	apiCall(t, f, "POST", path, `{"title":"Two"}`, 201)
	r := f.repo("adaiversen", "variants")
	for _, query := range []string{"", "state=open", "state=all&direction=asc", "per_page=1&page=1", "per_page=1&page=2"} {
		q := request{method: "GET", path: "/api/v3" + path, query: query, host: "forge.local", authorization: "Bearer test-token"}
		first := f.dispatch(q)
		second := f.dispatch(q)
		if !bytes.HasPrefix(first, []byte("HTTP/1.1 200 OK\r\n")) || &first[0] != &second[0] {
			t.Fatalf("query %q did not reuse the complete frame", query)
		}
	}
	if len(r.wire.Load().entries) != 5 {
		t.Fatal("query variants evicted each other")
	}
	q := request{method: "GET", path: "/api/v3" + path + "/1", host: "forge.local", authorization: "Bearer test-token"}
	first, second := f.dispatch(q), f.dispatch(q)
	if &first[0] != &second[0] {
		t.Fatal("detail response was copied on cache hit")
	}
	apiCall(t, f, "PATCH", path+"/1", `{"title":"Changed"}`, 200)
	if r.wire.Load() != nil || !bytes.Contains(f.dispatch(q), []byte("Changed")) || !bytes.Contains(first, []byte("One")) {
		t.Fatal("mutation did not detach immutable cached frames")
	}
}

func TestWireMemoCannotSurviveMutationDeletionOrAuthChange(t *testing.T) {
	t.Setenv("MAKEFUN_SHOP_TOKEN", "test-token")
	t.Setenv("MAKEFUN_FORGE_LOGIN", "adaiversen")
	f := newForge()
	apiCall(t, f, "POST", "/user/repos", `{"name":"memo"}`, 201)
	path := "/repos/adaiversen/memo/issues"
	apiCall(t, f, "POST", path, `{"title":"First"}`, 201)
	q := request{method: "GET", path: "/api/v3" + path + "/1", authorization: "Bearer test-token", host: "forge.local"}
	var memo wireReadMemo
	value := f.dispatch(q)
	memo.remember(f, q, value, nil)
	if hit := memo.lookup(f, q); len(hit) == 0 || &hit[0] != &value[0] {
		t.Fatal("keep-alive memo did not reuse its repository's complete frame")
	}
	changed := q
	changed.authorization = "Bearer wrong"
	if memo.lookup(f, changed) != nil {
		t.Fatal("memo bypassed authentication")
	}
	apiCall(t, f, "PATCH", path+"/1", `{"title":"Updated"}`, 200)
	if memo.lookup(f, q) != nil {
		t.Fatal("memo survived a mutation")
	}
	memo.remember(f, q, f.dispatch(q), nil)
	apiCall(t, f, "DELETE", "/repos/adaiversen/memo", "", 204)
	if memo.lookup(f, q) != nil {
		t.Fatal("memo survived repository deletion")
	}
	apiCall(t, f, "POST", "/user/repos", `{"name":"memo"}`, 201)
	if memo.lookup(f, q) != nil {
		t.Fatal("memo served the previous repository's response after recreation")
	}
}

func TestWireHeaderReuseFallsBackForChangedFraming(t *testing.T) {
	header := "GET /api/v3/repos/adaiversen/cache/issues/1 HTTP/1.1\r\nHost: forge.local\r\nAuthorization: Bearer test-token\r\n\r\n"
	reader := bufio.NewReader(strings.NewReader(header))
	var memo wireReadMemo
	q, n, chunked, expect, closeAfter, err, raw := memo.read(reader)
	if err != nil || n != 0 || chunked || expect || closeAfter || len(raw) == 0 {
		t.Fatalf("valid GET was not captured: %v", err)
	}
	memo.request, memo.header = q, bytes.Clone(raw)
	reader = bufio.NewReader(strings.NewReader(header + "GET /healthz HTTP/1.1\r\nHost: x\r\n\r\n"))
	got, _, _, _, _, err, _ := memo.read(reader)
	if err != nil || got.path != q.path || reader.Buffered() == 0 {
		t.Fatalf("identical header lost the pipelined request: %v", err)
	}
	got, _, _, _, _, err, _ = memo.read(reader)
	if err != nil || got.path != "/healthz" {
		t.Fatalf("shorter request was not parsed normally: %v", err)
	}
	for _, test := range []struct {
		head         string
		size         int
		chunk, close bool
	}{
		{strings.Replace(header, "\r\n\r\n", "\r\nContent-Length: 4\r\n\r\n", 1), 4, false, false},
		{strings.Replace(header, "\r\n\r\n", "\r\nTransfer-Encoding: chunked\r\n\r\n", 1), 0, true, false},
		{strings.Replace(header, "\r\n\r\n", "\r\nConnection: close\r\n\r\n", 1), 0, false, true},
	} {
		reader = bufio.NewReader(strings.NewReader(test.head))
		_, n, chunked, _, closeAfter, err, raw = memo.read(reader)
		if err != nil || n != test.size || chunked != test.chunk || closeAfter != test.close || raw != nil {
			t.Fatalf("changed framing bypassed parser: n=%d chunk=%v close=%v err=%v", n, chunked, closeAfter, err)
		}
	}
}

func TestKeepAliveRevalidatesStoreAndAuthorization(t *testing.T) {
	t.Setenv("MAKEFUN_SHOP_TOKEN", "test-token")
	t.Setenv("MAKEFUN_FORGE_LOGIN", "adaiversen")
	f := newForge()
	apiCall(t, f, "POST", "/user/repos", `{"name":"socket"}`, 201)
	path := "/repos/adaiversen/socket/issues"
	apiCall(t, f, "POST", path, `{"title":"Original"}`, 201)
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := listener.AcceptTCP()
		if err == nil {
			f.serve(conn)
		}
	}()
	conn, err := net.Dial("tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { conn.Close(); <-done }()
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	read := func(token string, status int, title string) {
		t.Helper()
		_, err := io.WriteString(conn, "GET /api/v3"+path+"/1 HTTP/1.1\r\nHost: forge.local\r\nAuthorization: Bearer "+token+"\r\n\r\n")
		if err != nil {
			t.Fatal(err)
		}
		res, err := http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil || res.StatusCode != status || !bytes.Contains(body, []byte(title)) {
			t.Fatalf("cached GET: status=%d body=%s err=%v", res.StatusCode, body, err)
		}
	}
	read("test-token", 200, "Original")
	read("test-token", 200, "Original")
	apiCall(t, f, "PATCH", path+"/1", `{"title":"Revised"}`, 200)
	read("test-token", 200, "Revised")
	read("wrong-token", 401, "Bad credentials")
	read("test-token", 200, "Revised")
}
