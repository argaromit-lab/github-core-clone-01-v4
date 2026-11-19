package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type oid [20]byte

var zero oid
var invalidPack = errors.New("invalid pack")

func idFromString(s string) oid {
	if len(s) != 40 {
		return zero
	}
	var id oid
	if _, err := hex.Decode(id[:], []byte(s)); err != nil {
		return zero
	}
	return id
}

func (id oid) String() string { return hex.EncodeToString(id[:]) }

const (
	commitKind  = 1
	treeKind    = 2
	blobKind    = 3
	tagKind     = 4
	offsetDelta = 6
	refDelta    = 7
)

var kindName = [...]string{"", "commit", "tree", "blob", "tag"}

type treeEntry struct {
	name, mode string
	id         oid
}

type gitObject struct {
	id            oid
	kind          int
	data, encoded []byte
	target        oid
	parents       []oid
	entries       []treeEntry
}

func constructObject(kind int, data, packed []byte) (*gitObject, error) {
	var head [32]byte
	key := append(head[:0], kindName[kind]...)
	key = append(key, ' ')
	key = strconv.AppendInt(key, int64(len(data)), 10)
	key = append(key, 0)
	h := sha1.New()
	_, _ = h.Write(key)
	_, _ = h.Write(data)
	g := &gitObject{kind: kind, data: data, encoded: packed}
	h.Sum(g.id[:0])
	switch kind {
	case commitKind, tagKind:
		for line := range bytes.SplitSeq(data, []byte{'\n'}) {
			if len(line) == 0 {
				break
			}
			if bytes.HasPrefix(line, []byte("tree ")) || bytes.HasPrefix(line, []byte("object ")) {
				_, value, _ := bytes.Cut(line, []byte{' '})
				g.target = idFromString(string(value))
			} else if kind == commitKind && bytes.HasPrefix(line, []byte("parent ")) {
				id := idFromString(string(line[7:]))
				if id == zero {
					return nil, invalidPack
				}
				g.parents = append(g.parents, id)
			}
		}
		if g.target == zero {
			return nil, invalidPack
		}
	case treeKind:
		for len(data) > 0 {
			name, rest, found := bytes.Cut(data, []byte{0})
			if !found || len(rest) < 20 {
				return nil, invalidPack
			}
			mode, path, valid := bytes.Cut(name, []byte{' '})
			if !valid || len(path) == 0 || bytes.ContainsAny(path, "/") {
				return nil, invalidPack
			}
			g.entries = append(g.entries, treeEntry{string(path), string(mode), oid(rest[:20])})
			data = rest[20:]
		}
	}
	return g, nil
}

type packedDelta struct {
	position, baseOffset int
	baseID               oid
	data                 []byte
}

var inflaterPool sync.Pool

func inflateEntry(input *bytes.Reader, out []byte) error {
	var zr io.ReadCloser
	var err error
	if saved := inflaterPool.Get(); saved != nil {
		zr = saved.(io.ReadCloser)
		err = zr.(zlib.Resetter).Reset(input, nil)
	} else {
		zr, err = zlib.NewReader(input)
	}
	if err != nil {
		return err
	}
	defer func() { _ = zr.Close(); inflaterPool.Put(zr) }()
	if _, err = io.ReadFull(zr, out); err != nil {
		return err
	}
	var extra [1]byte
	if n, err := zr.Read(extra[:]); n != 0 || err != io.EOF {
		return invalidPack
	}
	return nil
}

func deltaVarint(data []byte, pos *int) (int, bool) {
	value := 0
	for shift := 0; shift <= 28 && *pos < len(data); shift += 7 {
		b := data[*pos]
		*pos += 1
		value |= int(b&127) << shift
		if b < 128 {
			return value, true
		}
	}
	return 0, false
}

func applyDelta(base, instructions []byte) ([]byte, error) {
	pos := 0
	baseSize, ok := deltaVarint(instructions, &pos)
	if !ok || baseSize != len(base) {
		return nil, invalidPack
	}
	size, ok := deltaVarint(instructions, &pos)
	if !ok || size > maxBody {
		return nil, invalidPack
	}
	out := make([]byte, 0, size)
	for pos < len(instructions) {
		op := instructions[pos]
		pos++
		if op == 0 {
			return nil, invalidPack
		}
		if op&128 == 0 {
			n := int(op)
			if n > len(instructions)-pos || n > size-len(out) {
				return nil, invalidPack
			}
			out = append(out, instructions[pos:pos+n]...)
			pos += n
			continue
		}
		offset, length := 0, 0
		for i := 0; i < 7; i++ {
			if op&(1<<i) == 0 {
				continue
			}
			if pos >= len(instructions) {
				return nil, invalidPack
			}
			if i < 4 {
				offset |= int(instructions[pos]) << (8 * i)
			} else {
				length |= int(instructions[pos]) << (8 * (i - 4))
			}
			pos++
		}
		if length == 0 {
			length = 65536
		}
		if offset > len(base) || length > len(base)-offset || length > size-len(out) {
			return nil, invalidPack
		}
		out = append(out, base[offset:offset+length]...)
	}
	if len(out) != size {
		return nil, invalidPack
	}
	return out, nil
}

// Non-delta entries retain the original pack entry, including its zlib stream.
// The pack buffer is private, so later connection reads cannot mutate the store.
func ingest(pack []byte, existing map[oid]*gitObject) (map[oid]*gitObject, error) {
	if len(pack) == 0 {
		return nil, nil
	}
	if len(pack) < 32 || string(pack[:4]) != "PACK" {
		return nil, invalidPack
	}
	version := binary.BigEndian.Uint32(pack[4:8])
	count := int(binary.BigEndian.Uint32(pack[8:12]))
	limit := len(pack) - 20
	if (version != 2 && version != 3) || count > 200000 || count > (limit-12)/9 {
		return nil, invalidPack
	}
	if sha1.Sum(pack[:limit]) != oid(pack[limit:]) {
		return nil, invalidPack
	}
	pack = bytes.Clone(pack[:limit])
	objects := make(map[oid]*gitObject, count)
	offsets := make(map[int]*gitObject, count)
	var pending []packedDelta
	var reader bytes.Reader
	budget := 128 << 20
	pos := 12
	for i := 0; i < count; i++ {
		if pos >= limit {
			return nil, invalidPack
		}
		start := pos
		b := pack[pos]
		pos++
		kind, size := int(b>>4&7), int(b&15)
		for shift := 4; b >= 128; shift += 7 {
			if pos >= limit || shift >= 32 {
				return nil, invalidPack
			}
			b = pack[pos]
			pos++
			size |= int(b&127) << shift
		}
		delta := packedDelta{position: start}
		switch kind {
		case commitKind, treeKind, blobKind, tagKind:
		case offsetDelta:
			if pos >= limit {
				return nil, invalidPack
			}
			b = pack[pos]
			pos++
			dist := int(b & 127)
			for b >= 128 {
				if pos >= limit || dist >= start {
					return nil, invalidPack
				}
				b = pack[pos]
				pos++
				dist = (dist+1)<<7 | int(b&127)
			}
			if dist == 0 || start-dist < 12 {
				return nil, invalidPack
			}
			delta.baseOffset = start - dist
		case refDelta:
			if limit-pos < 20 {
				return nil, invalidPack
			}
			delta.baseID = oid(pack[pos : pos+20])
			pos += 20
		default:
			return nil, invalidPack
		}
		if size > maxBody || size > budget {
			return nil, invalidPack
		}
		budget -= size
		data := make([]byte, size)
		reader.Reset(pack[pos:limit])
		if err := inflateEntry(&reader, data); err != nil {
			return nil, invalidPack
		}
		pos = limit - reader.Len()
		if kind >= offsetDelta {
			delta.data = data
			pending = append(pending, delta)
			continue
		}
		g, err := constructObject(kind, data, pack[start:pos:pos])
		if err != nil {
			return nil, err
		}
		objects[g.id], offsets[start] = g, g
	}
	if pos != limit {
		return nil, invalidPack
	}
	for len(pending) > 0 {
		unresolved := pending[:0]
		for _, d := range pending {
			base := offsets[d.baseOffset]
			if d.baseOffset == 0 {
				base = objects[d.baseID]
				if base == nil {
					base = existing[d.baseID]
				}
			}
			if base == nil {
				unresolved = append(unresolved, d)
				continue
			}
			data, err := applyDelta(base.data, d.data)
			if err != nil || len(data) > budget {
				return nil, invalidPack
			}
			budget -= len(data)
			g, err := constructObject(base.kind, data, nil)
			if err != nil {
				return nil, err
			}
			objects[g.id], offsets[d.position] = g, g
		}
		if len(unresolved) == len(pending) {
			return nil, invalidPack
		}
		pending = unresolved
	}
	return objects, nil
}

var deflaterPool sync.Pool

func (g *gitObject) packEntry() []byte {
	if g.encoded != nil {
		return g.encoded
	}
	size := len(g.data)
	first := byte(g.kind<<4 | size&15)
	var output bytes.Buffer
	for size >>= 4; size != 0; size >>= 7 {
		_ = output.WriteByte(first | 128)
		first = byte(size & 127)
	}
	_ = output.WriteByte(first)
	var zw *zlib.Writer
	if saved := deflaterPool.Get(); saved != nil {
		zw = saved.(*zlib.Writer)
		zw.Reset(&output)
	} else {
		zw, _ = zlib.NewWriterLevel(&output, zlib.BestSpeed)
	}
	_, _ = zw.Write(g.data)
	_ = zw.Close()
	deflaterPool.Put(zw)
	g.encoded = output.Bytes()
	return g.encoded
}

func writePack(objects []*gitObject) []byte {
	return appendRawPack(nil, objects, false)
}

// Reuse validated receive-pack entries verbatim. Hash and frame their compressed
// slices directly, without another inflate/deflate cycle or an intermediate pack.
func appendRawPack(out []byte, objects []*gitObject, side bool) []byte {
	remaining := 32
	for _, g := range objects {
		remaining += len(g.packEntry())
	}
	var header [12]byte
	copy(header[:], "PACK")
	binary.BigEndian.PutUint32(header[4:8], 2)
	binary.BigEndian.PutUint32(header[8:12], uint32(len(objects)))
	hash := sha1.New()
	hash.Write(header[:])
	for _, g := range objects {
		hash.Write(g.encoded)
	}
	var checksum [20]byte
	hash.Sum(checksum[:0])
	capacity := remaining
	if side {
		capacity += ((remaining+65514)/65515)*5 + len(flush)
	}
	if cap(out)-len(out) < capacity {
		grown := make([]byte, len(out), len(out)+capacity)
		copy(grown, out)
		out = grown
	}
	bandRemaining := 0
	appendSlice := func(raw []byte) {
		if !side {
			out = append(out, raw...)
			return
		}
		for len(raw) > 0 {
			if bandRemaining == 0 {
				bandRemaining = min(remaining, 65515)
				out = append(pktHeader(out, bandRemaining+5), 1)
			}
			n := min(bandRemaining, len(raw))
			out = append(out, raw[:n]...)
			raw = raw[n:]
			bandRemaining -= n
			remaining -= n
		}
	}
	appendSlice(header[:])
	for _, g := range objects {
		appendSlice(g.encoded)
	}
	appendSlice(checksum[:])
	if side {
		out = append(out, flush...)
	}
	return out
}

func (r *repository) saveObject(kind int, data []byte) oid {
	g, err := constructObject(kind, data, nil)
	if err != nil {
		panic(err)
	}
	if r.objects[g.id] == nil {
		r.objects[g.id] = g
	}
	return g.id
}

func (r *repository) filesAt(id oid) map[string]treeEntry {
	files := make(map[string]treeEntry)
	if c := r.objects[id]; c != nil && c.kind == commitKind {
		var walk func(oid, string, int)
		walk = func(id oid, prefix string, depth int) {
			g := r.objects[id]
			if g == nil || depth > 256 {
				return
			}
			for _, entry := range g.entries {
				p := prefix + entry.name
				if entry.mode == "40000" || entry.mode == "040000" {
					walk(entry.id, p+"/", depth+1)
				} else {
					files[p] = entry
				}
			}
		}
		walk(c.target, "", 0)
	}
	return files
}

type directory struct {
	files map[string]treeEntry
	sub   map[string]*directory
}

func newDirectory() *directory { return &directory{map[string]treeEntry{}, map[string]*directory{}} }

func (r *repository) buildTree(files map[string]treeEntry) oid {
	root := newDirectory()
	for path, entry := range files {
		segments := strings.Split(path, "/")
		dir := root
		for _, part := range segments[:len(segments)-1] {
			if dir.sub[part] == nil {
				dir.sub[part] = newDirectory()
			}
			dir = dir.sub[part]
		}
		name := segments[len(segments)-1]
		entry.name = name
		dir.files[name] = entry
	}
	var serialize func(*directory) oid
	serialize = func(dir *directory) oid {
		order := make([]string, 0, len(dir.files)+len(dir.sub))
		for name := range dir.files {
			order = append(order, name)
		}
		for name := range dir.sub {
			order = append(order, name+"/")
		}
		sort.Strings(order)
		var data []byte
		for _, name := range order {
			entry, exists := dir.files[name]
			if !exists {
				name = strings.TrimSuffix(name, "/")
				entry = treeEntry{name, "40000", serialize(dir.sub[name])}
			}
			data = append(data, entry.mode...)
			data = append(data, ' ')
			data = append(data, name...)
			data = append(data, 0)
			data = append(data, entry.id[:]...)
		}
		return r.saveObject(treeKind, data)
	}
	return serialize(root)
}

func (r *repository) commitFiles(files map[string]treeEntry, parent oid, message, login string) oid {
	tree := r.buildTree(files)
	var data []byte
	data = append(data, "tree "+tree.String()+"\n"...)
	if parent != zero {
		data = append(data, "parent "+parent.String()+"\n"...)
	}
	identity := login + " <" + login + "@users.noreply.github.com> 946684800 +0000\n"
	data = append(data, "author "+identity+"committer "+identity+"\n"+strings.TrimRight(message, "\n")+"\n"...)
	return r.saveObject(commitKind, data)
}

func (r *repository) peel(id oid) oid {
	for steps := 0; steps < 64; steps++ {
		g := r.objects[id]
		if g == nil || g.kind != tagKind {
			return id
		}
		id = g.target
	}
	return zero
}

func (r *repository) resolve(ref string) oid {
	if ref == "" || ref == "HEAD" {
		ref = r.branch
	}
	if id := r.refs[ref]; id != zero {
		return id
	}
	if id := r.refs["refs/heads/"+ref]; id != zero {
		return id
	}
	if id := r.refs["refs/tags/"+ref]; id != zero {
		return r.peel(id)
	}
	id := idFromString(ref)
	if r.objects[id] != nil {
		return id
	}
	return zero
}

func (r *repository) ancestors(start oid) map[oid]bool {
	seen := make(map[oid]bool)
	queue := []oid{start}
	for len(queue) > 0 {
		id := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		g := r.objects[id]
		if g == nil || seen[id] {
			continue
		}
		seen[id] = true
		queue = append(queue, g.parents...)
	}
	return seen
}

func (r *repository) mergeBase(left, right oid) oid {
	seen := r.ancestors(left)
	queue := []oid{right}
	visited := make(map[oid]bool)
	for i := 0; i < len(queue); i++ {
		id := queue[i]
		if seen[id] {
			return id
		}
		if g := r.objects[id]; g != nil && !visited[id] {
			visited[id] = true
			queue = append(queue, g.parents...)
		}
	}
	return zero
}
