package main

import (
	"bufio"
	"bytes"
	"strings"
)

const (
	maxWireEntries = 256
	// Leave room for map buckets, keys and allocator overhead below 2 MiB.
	maxWireBytes = 1 << 20
	maxWireEntry = 64 << 10
)

// Each generation owns an immutable map of complete HTTP frames. Readers never
// lock; mutations atomically detach the generation under the repository lock.
type wireFrames struct {
	entries    map[string]response
	bytes      int
	generation *wireGeneration
}

type wireGeneration struct{ marker byte }

func (r *repository) cachedWire(key string) response {
	if frames := r.wire.Load(); frames != nil {
		return frames.entries[key]
	}
	return nil
}

// Call while holding r.mu (read or write): a publisher cannot outlive a mutation
// and accidentally attach stale bytes to the next generation.
func (r *repository) storeWire(key string, value response) response {
	if len(key) > 4096 || len(value) > maxWireEntry {
		return value
	}
	for {
		old := r.wire.Load()
		count, size := 0, 0
		if old != nil {
			if hit := old.entries[key]; hit != nil {
				return hit
			}
			count, size = len(old.entries), old.bytes
		}
		cost := len(key) + len(value) + 128
		if count >= maxWireEntries || size+cost > maxWireBytes {
			return value
		}
		next := &wireFrames{entries: make(map[string]response, count+1), bytes: size + cost}
		if old != nil {
			next.generation = old.generation
			for k, v := range old.entries {
				next.entries[k] = v
			}
		} else {
			next.generation = &wireGeneration{}
		}
		next.entries[key] = value
		if r.wire.CompareAndSwap(old, next) {
			return value
		}
	}
}

func (r *repository) invalidateWire() { r.wire.Store(nil) }

func issueWireKey(q request) string {
	// Both path and host matter for pagination's absolute Link header.
	return "issues:" + q.host + " " + q.path + "?" + q.query
}

func refWireKey(service string, version2 bool) string {
	if version2 {
		return "refs:v2:" + service
	}
	return "refs:v0:" + service
}

func issueReadAction(action string) bool {
	return action == "issues" || strings.HasPrefix(action, "issues/") && !strings.Contains(action[7:], "/")
}

// A persistent socket may remember one lookup into the same bounded repo cache.
// It never owns another response copy and must check the cache generation and
// repository-directory epoch on every use, as well as all auth/routing inputs.
type wireReadMemo struct {
	request    request
	repo       *repository
	generation *wireGeneration
	epoch      uint64
	value      response
	header     []byte
}

// Reuse the parsed form only when the entire buffered header is byte-for-byte
// identical to a previously validated, bodyless HTTP/1.1 GET. A short or changed
// request always follows the normal parser, without waiting for the old length.
func (memo *wireReadMemo) read(r *bufio.Reader) (q request, size int, chunked, expect, closeAfter bool, err error, header []byte) {
	if _, peekErr := r.Peek(1); peekErr == nil {
		buffered, _ := r.Peek(r.Buffered())
		if len(memo.header) != 0 && len(buffered) >= len(memo.header) && bytes.Equal(buffered[:len(memo.header)], memo.header) {
			_, err = r.Discard(len(memo.header))
			q = memo.request
			return
		}
		if bytes.HasPrefix(buffered, []byte("GET ")) {
			if end := bytes.Index(buffered, []byte("\r\n\r\n")); end >= 0 && end+4 <= 4096 {
				header = buffered[:end+4]
			}
		}
	}
	q, size, chunked, expect, closeAfter, err = readRequest(r)
	if size != 0 || chunked || expect || closeAfter {
		header = nil
	}
	return
}

func (memo *wireReadMemo) lookup(f *forge, q request) response {
	p := memo.request
	if memo.repo != nil && q.method == "GET" && len(q.body) == 0 &&
		q.path == p.path && q.query == p.query && q.host == p.host &&
		q.authorization == p.authorization && q.gitProtocol == p.gitProtocol &&
		f.repoEpoch.Load() == memo.epoch {
		if frames := memo.repo.wire.Load(); frames != nil && frames.generation == memo.generation {
			return memo.value
		}
	}
	return nil
}

func (memo *wireReadMemo) remember(f *forge, q request, value response, header []byte) {
	if header == nil && len(memo.header) != 0 && memo.request.path == q.path && memo.request.authorization == q.authorization && memo.request.query == q.query && memo.request.host == q.host && memo.request.gitProtocol == q.gitProtocol {
		header = memo.header
	}
	*memo = wireReadMemo{}
	if q.method != "GET" || len(q.body) != 0 {
		return
	}
	epoch := f.repoEpoch.Load()
	var r *repository
	var key string
	if tail, ok := strings.CutPrefix(q.path, "/api/v3/repos/"); ok {
		owner, tail, _ := strings.Cut(tail, "/")
		name, action, _ := strings.Cut(tail, "/")
		if issueReadAction(action) {
			r, key = f.repo(owner, name), issueWireKey(q)
		}
	}
	if r == nil {
		return
	}
	frames := r.wire.Load()
	if frames == nil {
		return
	}
	hit := frames.entries[key]
	if len(hit) == 0 || len(value) == 0 || &hit[0] != &value[0] || f.repoEpoch.Load() != epoch {
		return
	}
	q.body = nil
	*memo = wireReadMemo{request: q, repo: r, generation: frames.generation, epoch: epoch, value: hit, header: bytes.Clone(header)}
}
