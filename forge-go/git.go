package main

import (
	"bytes"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

const (
	upload  = "git-upload-pack"
	receive = "git-receive-pack"
	flush   = "0000"
	delim   = "0001"
)

func pktHeader(out []byte, n int) []byte {
	const hexChars = "0123456789abcdef"
	return append(out, hexChars[n>>12], hexChars[n>>8&15], hexChars[n>>4&15], hexChars[n&15])
}

func packet(out []byte, data string) []byte { return append(pktHeader(out, len(data)+4), data...) }

func sideband(out, data []byte) []byte {
	for len(data) > 0 {
		n := min(len(data), 65515)
		out = append(pktHeader(out, n+5), 1)
		out = append(out, data[:n]...)
		data = data[n:]
	}
	return append(out, flush...)
}

func takePacket(in []byte) (data, rest []byte, kind int) {
	if len(in) < 4 {
		return nil, nil, -1
	}
	n := 0
	for _, c := range in[:4] {
		n <<= 4
		switch {
		case c >= '0' && c <= '9':
			n += int(c - '0')
		case c >= 'a' && c <= 'f':
			n += int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			n += int(c-'A') + 10
		default:
			return nil, nil, -1
		}
	}
	if n < 3 {
		return nil, in[4:], n
	}
	if n < 4 || n > len(in) || n > 65520 {
		return nil, nil, -1
	}
	return in[4:n], in[n:], 3
}

func gitFrame(service, message string, body []byte) response {
	return frame(200, "application/x-"+service+"-"+message, "Cache-Control: no-cache\r\n", body)
}

var v2Advertisement = func() response {
	var out []byte
	for _, field := range []string{"version 2\n", "agent=static-forge/2\n", "ls-refs=unborn\n", "fetch=shallow\n", "object-format=sha1\n"} {
		out = packet(out, field)
	}
	return gitFrame(upload, "advertisement", append(out, flush...))
}()

type cloneFrame struct {
	id   oid
	v2   bool
	body response
}

func (f *forge) git(q request) response {
	owner, suffix, found := strings.Cut(strings.TrimPrefix(q.path, "/"), "/")
	if !found {
		return notFound
	}
	name, action, found := strings.Cut(suffix, "/")
	if !found || !strings.HasSuffix(name, ".git") {
		return notFound
	}
	r := f.repo(owner, strings.TrimSuffix(name, ".git"))
	if r == nil {
		return notFound
	}
	v2 := strings.Contains(q.gitProtocol, "version=2")
	if q.method == "GET" && action == "info/refs" {
		service := strings.TrimPrefix(q.query, "service=")
		if service != upload && service != receive {
			values, _ := url.ParseQuery(q.query)
			service = values.Get("service")
		}
		if service == upload && v2 {
			return v2Advertisement
		}
		if service != upload && service != receive {
			return notFound
		}
		r.mu.RLock()
		defer r.mu.RUnlock()
		return r.advertise(service)
	}
	if q.method != "POST" {
		return notFound
	}
	switch action {
	case receive:
		return r.receive(q.body)
	case upload:
		return r.upload(q.body, v2)
	default:
		return notFound
	}
}

func (r *repository) refNames() []string {
	names := make([]string, 0, len(r.refs))
	for name := range r.refs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (r *repository) advertise(service string) response {
	out := packet(nil, "# service="+service+"\n")
	out = append(out, flush...)
	caps := "report-status delete-refs side-band-64k ofs-delta"
	head := "refs/heads/" + r.branch
	if service == upload {
		caps = "side-band-64k shallow ofs-delta include-tag symref=HEAD:" + head
	}
	caps += " object-format=sha1 agent=static-forge/2"
	first := true
	writeRef := func(id oid, ref string) {
		if first {
			ref += "\x00" + caps
			first = false
		}
		out = packet(out, id.String()+" "+ref+"\n")
	}
	if service == upload && r.refs[head] != zero {
		writeRef(r.refs[head], "HEAD")
	}
	for _, name := range r.refNames() {
		id := r.refs[name]
		writeRef(id, name)
		if service == upload && strings.HasPrefix(name, "refs/tags/") {
			if target := r.peel(id); target != id && target != zero {
				writeRef(target, name+"^{}")
			}
		}
	}
	if first {
		writeRef(zero, "capabilities^{}")
	}
	return gitFrame(service, "advertisement", append(out, flush...))
}

func hasPrefix(name string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, prefix := range prefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func (r *repository) lsRefs(prefixes []string, symrefs, peel, unborn bool) response {
	var out []byte
	head := "refs/heads/" + r.branch
	if hasPrefix("HEAD", prefixes) {
		if id := r.refs[head]; id != zero {
			line := id.String() + " HEAD"
			if symrefs {
				line += " symref-target:" + head
			}
			out = packet(out, line+"\n")
		} else if unborn {
			out = packet(out, "unborn HEAD symref-target:"+head+"\n")
		}
	}
	for _, name := range r.refNames() {
		if !hasPrefix(name, prefixes) {
			continue
		}
		id := r.refs[name]
		line := id.String() + " " + name
		if peel && strings.HasPrefix(name, "refs/tags/") {
			if target := r.peel(id); target != id && target != zero {
				line += " peeled:" + target.String()
			}
		}
		out = packet(out, line+"\n")
	}
	return gitFrame(upload, "result", append(out, flush...))
}

func validRef(name string) bool {
	if (!strings.HasPrefix(name, "refs/heads/") && !strings.HasPrefix(name, "refs/tags/")) ||
		strings.Contains(name, "..") || strings.Contains(name, "@{") || strings.ContainsAny(name, " ~^:?*[\\") || strings.HasSuffix(name, ".") {
		return false
	}
	for _, c := range name {
		if c < 32 || c == 127 {
			return false
		}
	}
	for part := range strings.SplitSeq(name, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}

type refUpdate struct {
	oldID, newID oid
	name         string
}

func (r *repository) fastForward(old, next oid) bool {
	if old == zero || old == next {
		return true
	}
	first := r.objects[next]
	if first == nil || first.kind != commitKind {
		return false
	}
	for _, parent := range first.parents {
		if parent == old {
			return true
		}
	}
	return r.ancestors(next)[old]
}

func (r *repository) receive(body []byte) response {
	var updates []refUpdate
	side := false
	for {
		data, rest, kind := takePacket(body)
		body = rest
		if kind == 0 {
			break
		}
		if kind != 3 || len(updates) >= 1024 {
			return badRequest
		}
		command, caps, _ := bytes.Cut(data, []byte{0})
		if len(updates) == 0 {
			side = bytes.Contains(caps, []byte("side-band-64k"))
		}
		command = bytes.TrimSuffix(command, []byte{'\n'})
		if len(command) < 83 || command[40] != ' ' || command[81] != ' ' {
			return badRequest
		}
		oldID, newID := idFromString(string(command[:40])), idFromString(string(command[41:81]))
		if (oldID == zero && string(command[:40]) != zeroText) || (newID == zero && string(command[41:81]) != zeroText) {
			return badRequest
		}
		updates = append(updates, refUpdate{oldID, newID, string(command[82:])})
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	objects, err := ingest(body, r.objects)
	var out []byte
	if err != nil {
		out = packet(out, "unpack invalid pack\n")
	} else {
		out = packet(out, "unpack ok\n")
		for id, g := range objects {
			if r.objects[id] == nil {
				r.objects[id] = g
			}
		}
	}
	for _, update := range updates {
		reason := ""
		g := r.objects[update.newID]
		switch {
		case !validRef(update.name):
			reason = "invalid ref name"
		case err != nil:
			reason = "unpacker error"
		case r.refs[update.name] != update.oldID:
			reason = "non-fast-forward"
		case update.newID == zero:
			delete(r.refs, update.name)
		case g == nil:
			reason = "missing object"
		case strings.HasPrefix(update.name, "refs/heads/") && g.kind != commitKind:
			reason = "not a commit"
		case !r.fastForward(update.oldID, update.newID):
			reason = "non-fast-forward"
		default:
			r.refs[update.name] = update.newID
		}
		if reason != "" {
			out = packet(out, "ng "+update.name+" "+reason+"\n")
		} else {
			out = packet(out, "ok "+update.name+"\n")
		}
	}
	out = append(out, flush...)
	if side {
		out = sideband(nil, out)
	}
	return gitFrame(receive, "result", out)
}

const zeroText = "0000000000000000000000000000000000000000"

type fetchArgs struct {
	wants, haves, shallow            []oid
	depth                            int
	version2, side, done, includeTag bool
}

func (r *repository) upload(body []byte, v2 bool) response {
	var fetch fetchArgs
	command := "fetch"
	if v2 {
		command = ""
	}
	fetch.version2, fetch.side = v2, v2
	var prefixes []string
	var symrefs, peel, unborn bool
	for len(body) > 0 {
		data, rest, kind := takePacket(body)
		if kind < 0 {
			return badRequest
		}
		body = rest
		if kind != 3 {
			continue
		}
		line := bytes.TrimSuffix(data, []byte{'\n'})
		if bytes.HasPrefix(line, []byte("command=")) {
			command = string(line[8:])
			continue
		}
		key, arg, _ := bytes.Cut(line, []byte{' '})
		switch string(key) {
		case "symrefs":
			symrefs = true
		case "peel":
			peel = true
		case "unborn":
			unborn = true
		case "ref-prefix":
			prefixes = append(prefixes, string(arg))
		case "want":
			value, caps, _ := bytes.Cut(arg, []byte{' '})
			id := idFromString(string(value))
			if id == zero {
				return badRequest
			}
			fetch.wants = append(fetch.wants, id)
			fetch.side = fetch.side || bytes.Contains(caps, []byte("side-band"))
			fetch.includeTag = fetch.includeTag || bytes.Contains(caps, []byte("include-tag"))
		case "have":
			fetch.haves = append(fetch.haves, idFromString(string(arg)))
		case "shallow":
			fetch.shallow = append(fetch.shallow, idFromString(string(arg)))
		case "deepen":
			fetch.depth, _ = strconv.Atoi(string(arg))
		case "done":
			fetch.done = true
		case "include-tag":
			fetch.includeTag = true
		}
	}
	if command == "ls-refs" {
		r.mu.RLock()
		defer r.mu.RUnlock()
		return r.lsRefs(prefixes, symrefs, peel, unborn)
	}
	if command != "fetch" || len(fetch.wants) == 0 {
		return badRequest
	}
	cacheable := fetch.depth == 1 && len(fetch.wants) == 1 && len(fetch.haves) == 0 && len(fetch.shallow) == 0 && fetch.side && fetch.done && !fetch.includeTag
	if cacheable {
		if hit := r.shallow.Load(); hit != nil && hit.id == fetch.wants[0] && hit.v2 == v2 {
			return hit.body
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if cacheable {
		if hit := r.shallow.Load(); hit != nil && hit.id == fetch.wants[0] && hit.v2 == v2 {
			return hit.body
		}
	}
	result := r.fetch(fetch)
	if cacheable && r.objects[fetch.wants[0]] != nil {
		r.shallow.Store(&cloneFrame{fetch.wants[0], v2, result})
	}
	return result
}

// collect traverses commit history independently of trees and tag edges, so a
// shallow boundary never drops the snapshot of its final included commit.
func (r *repository) collect(roots []oid, depth int, stop map[oid]bool) ([]*gitObject, map[oid]bool) {
	type edge struct {
		id    oid
		level int
	}
	queue := make([]edge, 0, len(roots)+16)
	for _, root := range roots {
		queue = append(queue, edge{root, 1})
	}
	seen := make(map[oid]int)
	var out []*gitObject
	boundaries := make(map[oid]bool)
	for i := 0; i < len(queue); i++ {
		e := queue[i]
		g := r.objects[e.id]
		if g == nil || (seen[e.id] != 0 && seen[e.id] <= e.level) {
			continue
		}
		if seen[e.id] == 0 {
			out = append(out, g)
		}
		seen[e.id] = e.level
		switch g.kind {
		case commitKind:
			queue = append(queue, edge{g.target, e.level})
			if stop[e.id] || (depth > 0 && e.level >= depth) {
				if len(g.parents) > 0 {
					boundaries[e.id] = true
				}
			} else {
				delete(boundaries, e.id)
				for _, p := range g.parents {
					queue = append(queue, edge{p, e.level + 1})
				}
			}
		case tagKind:
			queue = append(queue, edge{g.target, e.level})
		case treeKind:
			for _, entry := range g.entries {
				if entry.mode != "160000" {
					queue = append(queue, edge{entry.id, e.level})
				}
			}
		}
	}
	return out, boundaries
}

func (r *repository) fetch(args fetchArgs) response {
	clientBoundary := make(map[oid]bool)
	for _, id := range args.shallow {
		clientBoundary[id] = true
	}
	haveRoots := append(append([]oid(nil), args.haves...), args.shallow...)
	have, _ := r.collect(haveRoots, 0, clientBoundary)
	held := make(map[oid]bool, len(have))
	for _, g := range have {
		held[g.id] = true
	}
	for _, id := range args.wants {
		if r.objects[id] == nil {
			return gitFrame(upload, "result", packet(nil, "ERR unknown object\n"))
		}
	}
	objects, boundary := r.collect(args.wants, args.depth, nil)
	toSend := objects[:0]
	for _, g := range objects {
		if !held[g.id] {
			toSend = append(toSend, g)
		}
	}
	if args.includeTag {
		included := make(map[oid]bool, len(toSend))
		for _, g := range toSend {
			included[g.id] = true
		}
		for _, name := range r.refNames() {
			id := r.refs[name]
			if g := r.objects[id]; g != nil && g.kind == tagKind && !held[id] && !included[id] && included[r.peel(id)] {
				toSend = append(toSend, g)
				included[id] = true
			}
		}
	}
	var out []byte
	if !args.done && args.depth == 0 {
		if args.version2 {
			out = packet(out, "acknowledgments\n")
		}
		n := 0
		for _, id := range args.haves {
			if r.objects[id] != nil && args.version2 {
				out = packet(out, "ACK "+id.String()+"\n")
				n++
			}
		}
		if n == 0 {
			out = packet(out, "NAK\n")
		}
		if args.version2 {
			out = append(out, flush...)
		}
		return gitFrame(upload, "result", out)
	}
	if args.depth > 0 {
		var shallowLines []string
		for id := range boundary {
			if !clientBoundary[id] {
				shallowLines = append(shallowLines, "shallow "+id.String()+"\n")
			}
		}
		for id := range clientBoundary {
			if !boundary[id] && (len(args.wants) == 1 && args.wants[0] == id || args.depth > 1) {
				shallowLines = append(shallowLines, "unshallow "+id.String()+"\n")
			}
		}
		sort.Strings(shallowLines)
		if len(shallowLines) != 0 || !args.version2 {
			if args.version2 {
				out = packet(out, "shallow-info\n")
			}
			for _, line := range shallowLines {
				out = packet(out, line)
			}
			if args.version2 {
				out = append(out, delim...)
			} else {
				out = append(out, flush...)
				if !args.done {
					return gitFrame(upload, "result", out)
				}
			}
		}
	}
	if args.version2 {
		out = packet(out, "packfile\n")
	} else {
		ack := "NAK\n"
		for _, id := range args.haves {
			if r.objects[id] != nil {
				ack = "ACK " + id.String() + "\n"
				break
			}
		}
		out = packet(out, ack)
	}
	out = appendRawPack(out, toSend, args.side)
	return gitFrame(upload, "result", out)
}
