// Packfiles (gitprotocol-pack): reading packs the client pushes (deltas
// resolved, trailer verified) and writing packs the server fetches (full
// objects only, no deltas — the client resolves deltas, but nothing in the
// suite requires the server to send any, and plain objects keep the fetch path
// fast and simple).
//
// Layout: "PACK" + version u32be (2) + count u32be, then one entry per object:
// a type/size varint header + a zlib stream, then a 20-byte SHA-1 trailer over
// everything before it. Type codes: commit 1, tree 2, blob 3, tag 4, ofs-delta
// 6, ref-delta 7.

package main

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sync"
)

// The packfile type codes.
var typeCode = map[GitObjectType]byte{objCommit: 1, objTree: 2, objBlob: 3, objTag: 4}
var codeType = map[byte]GitObjectType{1: objCommit, 2: objTree, 3: objBlob, 4: objTag}

// packEntry is one parsed pack entry, before delta resolution.
type packEntry struct {
	offset   int
	typeCode byte
	size     int
	// The raw (delta) payload, inflated.
	data  []byte
	delta *packDelta
}

type packDelta struct {
	kind       string // "ofs" or "ref"
	baseOffset int
	baseOID    string
}

// PackReadResult is what ReadPack returns: the resolved objects and the pack's
// own metadata.
type PackReadResult struct {
	Version       int
	DeclaredCount int
	Objects       map[string]*GitObject
	DeltaCount    int
	ByteLength    int
	ChecksumOK    bool
}

// entryHeader is the decoded type/size varint header of one entry.
type entryHeader struct {
	typeCode byte
	size     int
	next     int
}

// readEntryHeader reads the type/size varint header at offset.
func readEntryHeader(pack []byte, offset int) (entryHeader, error) {
	var out entryHeader
	if offset >= len(pack) {
		return out, errors.New("pack-truncated: entry header past the end")
	}
	first := pack[offset]
	out.typeCode = (first >> 4) & 0x07
	out.size = int(first & 0x0f)
	shift := uint(4)
	at := offset + 1
	b := first
	for b&0x80 != 0 {
		if at >= len(pack) {
			return out, errors.New("pack-truncated: size varint past the end")
		}
		b = pack[at]
		out.size |= int(b&0x7f) << shift
		shift += 7
		at++
		if shift > 64 {
			return out, errors.New("pack-corrupt: size varint too long")
		}
	}
	out.next = at
	return out, nil
}

// readOffsetVarint reads the OFS_DELTA offset varint at offset.
func readOffsetVarint(pack []byte, offset int) (value, next int, err error) {
	if offset >= len(pack) {
		return 0, 0, errors.New("pack-truncated: offset varint past the end")
	}
	b := pack[offset]
	value = int(b & 0x7f)
	at := offset + 1
	for b&0x80 != 0 {
		if at >= len(pack) {
			return 0, 0, errors.New("pack-truncated: offset varint past the end")
		}
		b = pack[at]
		value = ((value + 1) * 128) + int(b&0x7f)
		at++
	}
	return value, at, nil
}

// readDeltaVarint reads one delta-stream size varint (7-bit little-endian
// groups).
func readDeltaVarint(data []byte, offset int) (value, next int, err error) {
	shift := uint(0)
	at := offset
	for {
		if at >= len(data) {
			return 0, 0, errors.New("delta-corrupt: size varint past the end")
		}
		b := data[at]
		value |= int(b&0x7f) << shift
		shift += 7
		at++
		if b&0x80 == 0 {
			break
		}
		if shift > 64 {
			return 0, 0, errors.New("delta-corrupt: size varint too long")
		}
	}
	return value, at, nil
}

// ApplyDelta applies a delta stream to its base bytes, returning the
// reconstructed object.
func ApplyDelta(base, delta []byte) ([]byte, error) {
	source, next, err := readDeltaVarint(delta, 0)
	if err != nil {
		return nil, err
	}
	if source != len(base) {
		return nil, errors.New("delta-base-size: the delta names another base size")
	}
	target, at, err := readDeltaVarint(delta, next)
	if err != nil {
		return nil, err
	}
	var out []byte
	produced := 0
	for at < len(delta) {
		opcode := delta[at]
		at++
		if opcode == 0 {
			return nil, errors.New("delta-corrupt: opcode 0")
		}
		if opcode&0x80 != 0 {
			// Copy from the base: bits 0-3 select offset bytes, bits 4-6
			// select size bytes.
			offset := 0
			size := 0
			for bit := 0; bit < 4; bit++ {
				if opcode&(1<<uint(bit)) != 0 {
					if at >= len(delta) {
						return nil, errors.New("delta-corrupt: copy offset past the end")
					}
					offset |= int(delta[at]) << (uint(bit) * 8)
					at++
				}
			}
			for bit := 0; bit < 3; bit++ {
				if opcode&(0x10<<uint(bit)) != 0 {
					if at >= len(delta) {
						return nil, errors.New("delta-corrupt: copy size past the end")
					}
					size |= int(delta[at]) << (uint(bit) * 8)
					at++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if offset+size > len(base) {
				return nil, errors.New("delta-corrupt: copy past the base")
			}
			out = append(out, base[offset:offset+size]...)
			produced += size
		} else {
			// Insert the next `opcode` literal bytes.
			if at+int(opcode) > len(delta) {
				return nil, errors.New("delta-corrupt: insert past the end")
			}
			out = append(out, delta[at:at+int(opcode)]...)
			produced += int(opcode)
			at += int(opcode)
		}
	}
	if produced != target {
		return nil, errors.New("delta-size: the reconstructed size differs")
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Pooled zlib inflate: the load-bearing optimization from the spike. A fresh
// zlib.NewReader per entry costs ~5x the inflate itself (reader setup +
// buffer growth); a sync.Pool of readers reset onto each entry's bytes makes
// the Go push path ~2.7x faster than Node's inflateSync. A bytes.Reader
// implements io.ByteReader, so the flate reader consumes bytes one at a time
// with no read-ahead — after inflating to EOF, the bytes.Reader's position is
// the exact consumed length, with no binary search and no engine internals.

var zlibSeedStream []byte

func init() {
	var b bytes.Buffer
	w := zlib.NewWriter(&b)
	w.Write([]byte("x"))
	w.Close()
	zlibSeedStream = b.Bytes()
}

var zlibReaderPool = sync.Pool{
	New: func() any {
		r, err := zlib.NewReader(bytes.NewReader(zlibSeedStream))
		if err != nil {
			panic(err)
		}
		return r
	},
}

// inflateEntry inflates the zlib stream at the start of b, returning the
// inflated bytes and the exact number of input bytes the stream consumed.
// It errors when the stream does not inflate at all.
func inflateEntry(b []byte) (data []byte, consumed int, err error) {
	r := zlibReaderPool.Get().(io.ReadCloser)
	defer zlibReaderPool.Put(r)
	br := bytes.NewReader(b)
	if err := r.(flate.Resetter).Reset(br, nil); err != nil {
		return nil, 0, errors.New("pack-zlib: the entry does not inflate")
	}
	// Read to EOF: the checksum is verified as part of the stream.
	data, err = io.ReadAll(r)
	if err != nil {
		return nil, 0, errors.New("pack-zlib: the entry does not inflate")
	}
	if err := r.Close(); err != nil {
		return nil, 0, errors.New("pack-zlib: the entry does not inflate")
	}
	// bytes.Reader is a ByteReader, so flate consumed exactly the stream.
	consumed = len(b) - br.Len()
	return data, consumed, nil
}

// ReadPack parses a packfile and resolves every entry (deltas included) to its
// object. resolveBase supplies a REF_DELTA base the pack does not hold (a thin
// pack's base, held by the reader). It errors on any structural fault: bad
// header, bad version, bad trailer, bad zlib, size mismatch, stray bytes, an
// unresolvable delta, or a delta cycle.
func ReadPack(pack []byte, resolveBase func(oid string) *GitObject) (*PackReadResult, error) {
	if len(pack) < 32 {
		return nil, errors.New("pack-truncated: a pack is at least 32 bytes")
	}
	if pack[0] != 'P' || pack[1] != 'A' || pack[2] != 'C' || pack[3] != 'K' {
		return nil, errors.New("pack-header: not a packfile")
	}
	version := int(binary.BigEndian.Uint32(pack[4:8]))
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("pack-version: %d", version)
	}
	declaredCount := int(binary.BigEndian.Uint32(pack[8:12]))
	trailerStart := len(pack) - 20
	trailer := pack[trailerStart:]
	sum := sha1.Sum(pack[:trailerStart])
	if hex.EncodeToString(sum[:]) != hex.EncodeToString(trailer) {
		return nil, errors.New("pack-checksum: the trailer does not match the bytes")
	}

	var entries []*packEntry
	offset := 12
	deltaCount := 0
	for index := 0; index < declaredCount; index++ {
		entryOffset := offset
		header, err := readEntryHeader(pack, offset)
		if err != nil {
			return nil, err
		}
		offset = header.next
		var delta *packDelta
		if header.typeCode == 6 {
			base, next, err := readOffsetVarint(pack, offset)
			if err != nil {
				return nil, err
			}
			baseOffset := entryOffset - base
			if baseOffset <= 0 || baseOffset >= entryOffset {
				return nil, errors.New("pack-delta: bad ofs-delta base offset")
			}
			delta = &packDelta{kind: "ofs", baseOffset: baseOffset}
			offset = next
			deltaCount++
		} else if header.typeCode == 7 {
			if offset+20 > trailerStart {
				return nil, errors.New("pack-truncated: ref-delta base oid past the end")
			}
			delta = &packDelta{kind: "ref", baseOID: hex.EncodeToString(pack[offset : offset+20])}
			offset += 20
			deltaCount++
		} else if _, ok := codeType[header.typeCode]; !ok {
			return nil, fmt.Errorf("pack-type: unknown type code %d", header.typeCode)
		}
		rest := pack[offset:trailerStart]
		inflated, consumed, err := inflateEntry(rest)
		if err != nil {
			return nil, err
		}
		if len(inflated) != header.size {
			return nil, errors.New("pack-size: the inflated size differs from the header")
		}
		if consumed == 0 {
			return nil, errors.New("pack-zlib: the entry consumed no bytes")
		}
		entries = append(entries, &packEntry{offset: entryOffset, typeCode: header.typeCode, size: header.size, data: inflated, delta: delta})
		offset += consumed
	}
	if offset != trailerStart {
		return nil, errors.New("pack-trailing: bytes after the last entry")
	}

	// The common case (every pack the suite pushes is delta-free): no delta
	// machinery at all, just hash each entry straight into the result.
	if deltaCount == 0 {
		objects := map[string]*GitObject{}
		for _, entry := range entries {
			object := &GitObject{Type: codeType[entry.typeCode], Data: entry.data}
			objects[HashObject(object.Type, object.Data)] = object
		}
		return &PackReadResult{Version: version, DeclaredCount: declaredCount, Objects: objects, DeltaCount: deltaCount, ByteLength: len(pack), ChecksumOK: true}, nil
	}

	// Resolve deltas, recursively, with cycle detection.
	byOffset := map[int]*packEntry{}
	for _, entry := range entries {
		byOffset[entry.offset] = entry
	}
	resolved := map[int]*GitObject{}
	resolving := map[int]bool{}
	var resolve func(entry *packEntry) (*GitObject, error)
	resolve = func(entry *packEntry) (*GitObject, error) {
		if done, ok := resolved[entry.offset]; ok {
			return done, nil
		}
		if resolving[entry.offset] {
			return nil, errors.New("pack-delta-cycle")
		}
		resolving[entry.offset] = true
		var object *GitObject
		if entry.delta == nil {
			object = &GitObject{Type: codeType[entry.typeCode], Data: entry.data}
		} else {
			var base *GitObject
			if entry.delta.kind == "ofs" {
				baseEntry, ok := byOffset[entry.delta.baseOffset]
				if !ok {
					return nil, errors.New("pack-delta-base: ofs-delta base not in the pack")
				}
				var err error
				base, err = resolve(baseEntry)
				if err != nil {
					return nil, err
				}
			} else {
				if resolveBase != nil {
					base = resolveBase(entry.delta.baseOID)
				}
				if base == nil {
					// A ref-delta base may also be an entry of this pack:
					// find it by its resolved oid.
					for _, candidate := range entries {
						if candidate.delta != nil {
							continue
						}
						if HashObject(codeType[candidate.typeCode], candidate.data) == entry.delta.baseOID {
							var err error
							base, err = resolve(candidate)
							if err != nil {
								return nil, err
							}
							break
						}
					}
				}
				if base == nil {
					return nil, fmt.Errorf("pack-delta-base: ref-delta base %s is nowhere", entry.delta.baseOID)
				}
			}
			data, err := ApplyDelta(base.Data, entry.data)
			if err != nil {
				return nil, err
			}
			object = &GitObject{Type: base.Type, Data: data}
		}
		delete(resolving, entry.offset)
		resolved[entry.offset] = object
		return object, nil
	}
	objects := map[string]*GitObject{}
	for _, entry := range entries {
		object, err := resolve(entry)
		if err != nil {
			return nil, err
		}
		objects[HashObject(object.Type, object.Data)] = object
	}
	return &PackReadResult{Version: version, DeclaredCount: declaredCount, Objects: objects, DeltaCount: deltaCount, ByteLength: len(pack), ChecksumOK: true}, nil
}

// entryHeaderBytes encodes one entry's type/size varint header.
func entryHeaderBytes(typeCode byte, size int) []byte {
	var out []byte
	first := byte(int(typeCode)<<4 | (size & 0x0f))
	rest := size >> 4
	if rest > 0 {
		first |= 0x80
	}
	out = append(out, first)
	for rest > 0 {
		next := byte(rest & 0x7f)
		rest >>= 7
		if rest > 0 {
			next |= 0x80
		}
		out = append(out, next)
	}
	return out
}

// WritePack builds a packfile (version 2) from full objects, in the given
// order, each zlib-deflated on its own. No deltas: every entry is a complete
// object, so the reader never needs a base.
func WritePack(objects []*GitObject) []byte {
	var body []byte
	header := make([]byte, 12)
	copy(header, "PACK")
	binary.BigEndian.PutUint32(header[4:8], 2)
	binary.BigEndian.PutUint32(header[8:12], uint32(len(objects)))
	body = append(body, header...)
	for _, object := range objects {
		body = append(body, entryHeaderBytes(typeCode[object.Type], len(object.Data))...)
		var buf bytes.Buffer
		w := zlib.NewWriter(&buf)
		w.Write(object.Data)
		w.Close()
		body = append(body, buf.Bytes()...)
	}
	sum := sha1.Sum(body)
	body = append(body, sum[:]...)
	return body
}

// VerifyPackChecksum returns the trailer of a pack (its last 20 bytes) as hex,
// and whether it matches the bytes.
func VerifyPackChecksum(pack []byte) (ok bool, expected, actual string) {
	if len(pack) < 20 {
		return false, "", ""
	}
	actual = hex.EncodeToString(pack[len(pack)-20:])
	sum := sha1.Sum(pack[:len(pack)-20])
	expected = hex.EncodeToString(sum[:])
	return actual == expected, expected, actual
}
