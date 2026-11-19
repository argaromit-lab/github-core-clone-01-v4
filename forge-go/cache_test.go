package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
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
	refsBody := append(packet(nil, "command=ls-refs\n"), delim...)
	refsBody = append(packet(refsBody, "ref-prefix refs/heads/main\n"), flush...)
	refs := request{method: "POST", path: "/adaiversen/cache.git/git-upload-pack", gitProtocol: "version=2", authorization: "Bearer test-token", body: refsBody}
	refsBefore := f.dispatch(refs)
	if !bytes.Equal(refsBefore, f.dispatch(refs)) {
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

func TestConcurrentCachePublishersKeepEveryEntry(t *testing.T) {
	r := &repository{}
	var workers sync.WaitGroup
	for i := 0; i < 64; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			key := string(rune('A' + i))
			r.storeFrame(key, response(key))
		}(i)
	}
	workers.Wait()
	for i := 0; i < 64; i++ {
		key := string(rune('A' + i))
		if string(r.cachedFrame(key)) != key {
			t.Fatalf("concurrent publisher lost entry %q", key)
		}
	}
}

func TestConnectionCacheRevalidatesStoreAndAuthorization(t *testing.T) {
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
