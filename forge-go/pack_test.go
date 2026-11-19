package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"testing"
)

func TestCompressedEntriesPassThroughUnchanged(t *testing.T) {
	data := bytes.Repeat([]byte("keep the original compressed representation\n"), 4096)
	g, err := constructObject(blobKind, data, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Use a different compression level than the forge's fallback encoder.
	var compressed bytes.Buffer
	writer, _ := zlib.NewWriterLevel(&compressed, zlib.BestCompression)
	writer.Write(data)
	writer.Close()
	size := len(data)
	first := byte(blobKind<<4 | size&15)
	var entry []byte
	for size >>= 4; size != 0; size >>= 7 {
		entry = append(entry, first|128)
		first = byte(size & 127)
	}
	entry = append(entry, first)
	entry = append(entry, compressed.Bytes()...)
	pack := make([]byte, 12)
	copy(pack, "PACK")
	binary.BigEndian.PutUint32(pack[4:8], 2)
	binary.BigEndian.PutUint32(pack[8:12], 1)
	pack = append(pack, entry...)
	sum := sha1.Sum(pack)
	pack = append(pack, sum[:]...)
	objects, err := ingest(pack, nil)
	if err != nil {
		t.Fatal(err)
	}
	stored := objects[g.id]
	if stored == nil || !bytes.Equal(stored.data, data) || !bytes.Equal(stored.encoded, entry) {
		t.Fatal("receive-pack changed the validated compressed entry")
	}
	// A later connection read can overwrite the incoming buffer safely.
	pack[12] ^= 255
	if !bytes.Equal(stored.encoded, entry) {
		t.Fatal("stored pack bytes alias the mutable request buffer")
	}
	// Outgoing pack generation must use only the retained compressed slices.
	stored.data = nil
	out := writePack([]*gitObject{stored})
	if !bytes.Equal(out[12:len(out)-20], entry) || sha1.Sum(out[:len(out)-20]) != oid(out[len(out)-20:]) {
		t.Fatal("upload-pack recompressed or corrupted the original entry")
	}
}

func TestRawPackSidebandsCrossObjectBoundaries(t *testing.T) {
	objects := make([]*gitObject, 3)
	for i := range objects {
		data := make([]byte, 70000+i)
		// Poorly compressible deterministic input crosses multiple 64 KiB bands.
		state := uint32(i + 1)
		for j := range data {
			state ^= state << 13
			state ^= state >> 17
			state ^= state << 5
			data[j] = byte(state)
		}
		objects[i], _ = constructObject(blobKind, data, nil)
	}
	want := writePack(objects)
	framed := appendRawPack(nil, objects, true)
	var unpacked []byte
	for len(framed) > 0 {
		payload, rest, kind := takePacket(framed)
		framed = rest
		if kind == 0 {
			if len(framed) != 0 {
				t.Fatal("bytes after the final flush")
			}
			break
		}
		if kind != 3 || len(payload) == 0 || payload[0] != 1 {
			t.Fatal("invalid sideband packet")
		}
		unpacked = append(unpacked, payload[1:]...)
	}
	if !bytes.Equal(unpacked, want) {
		t.Fatal("sideband framing changed the raw pack")
	}
}
