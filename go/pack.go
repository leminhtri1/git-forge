package main

// Packfiles: reading every entry type (including OFS_DELTA and REF_DELTA) and writing packs of whole
// objects. Compressed entries are cached per object id, so serving an object again costs a copy.

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
)

var typeByCode = map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}
var codeByType = map[string]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}

const (
	ofsDelta = 6
	refDelta = 7
)

type ParsedObject struct {
	ID   string
	Type string
	Data []byte
}

// countingReader tracks how many compressed bytes zlib consumed.
type countingReader struct {
	data []byte
	at   int
}

func (r *countingReader) Read(p []byte) (int, error) {
	if r.at >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.at:])
	r.at += n
	return n, nil
}

func (r *countingReader) ReadByte() (byte, error) {
	if r.at >= len(r.data) {
		return 0, io.EOF
	}
	b := r.data[r.at]
	r.at++
	return b, nil
}

// One inflater and one deflater, reset per object: building them fresh allocates tens to hundreds of
// KiB each, and handlers run one at a time under the global lock, so a single instance of each serves.
var (
	sharedInflateSrc = &countingReader{}
	sharedInflater   io.ReadCloser
	inflateProbe     = make([]byte, 1)
	sharedDeflateDst bytes.Buffer
	sharedDeflater   *zlib.Writer
)

// inflateAt inflates one zlib stream at offset into exactly size bytes (the pack entry header's size);
// it returns the data and the compressed length used, having checked the stream's adler32 trailer.
func inflateAt(buf []byte, offset int, size int) ([]byte, int, error) {
	src := sharedInflateSrc
	src.data, src.at = buf[offset:], 0
	if sharedInflater == nil {
		zr, err := zlib.NewReader(src)
		if err != nil {
			return nil, 0, err
		}
		sharedInflater = zr
	} else if err := sharedInflater.(zlib.Resetter).Reset(src, nil); err != nil {
		return nil, 0, err
	}
	out := make([]byte, size)
	if _, err := io.ReadFull(sharedInflater, out); err != nil {
		return nil, 0, err
	}
	// The stream must end exactly here: one more read reaches EOF and verifies the checksum.
	if n, err := sharedInflater.Read(inflateProbe); n != 0 || err != io.EOF {
		if err == nil || err == io.EOF {
			err = fmt.Errorf("zlib stream longer than its declared size %d", size)
		}
		return nil, 0, err
	}
	return out, src.at, nil
}

// deflate compresses data with the shared writer; the result is a fresh slice.
func deflate(data []byte) []byte {
	sharedDeflateDst.Reset()
	if sharedDeflater == nil {
		sharedDeflater, _ = zlib.NewWriterLevel(&sharedDeflateDst, zlib.BestSpeed)
	} else {
		sharedDeflater.Reset(&sharedDeflateDst)
	}
	_, _ = sharedDeflater.Write(data)
	_ = sharedDeflater.Close()
	return append([]byte(nil), sharedDeflateDst.Bytes()...)
}

func applyDelta(base, delta []byte) ([]byte, error) {
	at := 0
	readSize := func() int {
		size, shift := 0, uint(0)
		for at < len(delta) {
			b := delta[at]
			at++
			size |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return size
	}
	if readSize() != len(base) {
		return nil, fmt.Errorf("delta base size mismatch")
	}
	resultSize := readSize()
	out := make([]byte, 0, resultSize)
	for at < len(delta) {
		op := delta[at]
		at++
		if op&0x80 != 0 {
			off, size := 0, 0
			for i, bit := range []byte{0x01, 0x02, 0x04, 0x08} {
				if op&bit != 0 {
					off |= int(delta[at]) << (8 * uint(i))
					at++
				}
			}
			for i, bit := range []byte{0x10, 0x20, 0x40} {
				if op&bit != 0 {
					size |= int(delta[at]) << (8 * uint(i))
					at++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			if off+size > len(base) {
				return nil, fmt.Errorf("delta copy out of range")
			}
			out = append(out, base[off:off+size]...)
		} else if op > 0 {
			n := int(op)
			if at+n > len(delta) {
				return nil, fmt.Errorf("delta insert out of range")
			}
			out = append(out, delta[at:at+n]...)
			at += n
		} else {
			return nil, fmt.Errorf("delta opcode 0 is reserved")
		}
	}
	if len(out) != resultSize {
		return nil, fmt.Errorf("delta result size mismatch")
	}
	return out, nil
}

// readPack reads a whole pack; lookup resolves a REF_DELTA base the pack itself does not carry.
func readPack(pack []byte, lookup func(string) *GitObject) (objects []ParsedObject, err error) {
	defer func() {
		if r := recover(); r != nil {
			objects, err = nil, fmt.Errorf("malformed pack: %v", r)
		}
	}()
	if len(pack) < 32 || string(pack[:4]) != "PACK" {
		return nil, fmt.Errorf("missing PACK signature")
	}
	version := binary.BigEndian.Uint32(pack[4:8])
	if version != 2 && version != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", version)
	}
	count := int(binary.BigEndian.Uint32(pack[8:12]))
	sum := sha1.Sum(pack[:len(pack)-20])
	if !bytes.Equal(sum[:], pack[len(pack)-20:]) {
		return nil, fmt.Errorf("pack checksum mismatch")
	}
	type entry struct {
		offset     int
		typ        string
		data       []byte
		baseOffset int
		baseID     string
	}
	entries := make([]*entry, 0, count)
	byOffset := make(map[int]*entry, count)
	at := 12
	end := len(pack) - 20
	for i := 0; i < count; i++ {
		if at >= end {
			return nil, fmt.Errorf("pack ends before its declared object count")
		}
		start := at
		b := pack[at]
		at++
		code := int(b>>4) & 7
		size := int(b & 0x0f)
		shift := uint(4)
		for b&0x80 != 0 {
			b = pack[at]
			at++
			size |= int(b&0x7f) << shift
			shift += 7
		}
		e := &entry{offset: start, baseOffset: -1}
		switch code {
		case ofsDelta:
			b = pack[at]
			at++
			rel := int(b & 0x7f)
			for b&0x80 != 0 {
				b = pack[at]
				at++
				rel = (rel+1)<<7 | int(b&0x7f)
			}
			e.baseOffset = start - rel
		case refDelta:
			e.baseID = hex.EncodeToString(pack[at : at+20])
			at += 20
		default:
			t, ok := typeByCode[code]
			if !ok {
				return nil, fmt.Errorf("unknown pack object type %d", code)
			}
			e.typ = t
		}
		if size > 128<<20 {
			return nil, fmt.Errorf("object at %d declares %d bytes", start, size)
		}
		data, used, err := inflateAt(pack, at, size)
		if err != nil {
			return nil, fmt.Errorf("object at %d: %v", start, err)
		}
		at += used
		e.data = data
		entries = append(entries, e)
		byOffset[start] = e
	}

	type result struct {
		typ, id string
		data    []byte
	}
	resolved := make(map[*entry]*result, len(entries))
	byID := make(map[string]*result)
	var resolve func(e *entry, depth int) (*result, error)
	resolve = func(e *entry, depth int) (*result, error) {
		if r, ok := resolved[e]; ok {
			return r, nil
		}
		if depth > 10000 {
			return nil, fmt.Errorf("delta chain too deep")
		}
		var r *result
		if e.typ != "" {
			r = &result{typ: e.typ, data: e.data, id: hashObject(e.typ, e.data)}
		} else {
			var baseType string
			var baseData []byte
			if e.baseOffset >= 0 {
				be, ok := byOffset[e.baseOffset]
				if !ok {
					return nil, fmt.Errorf("ofs-delta base at %d is not in the pack", e.baseOffset)
				}
				br, err := resolve(be, depth+1)
				if err != nil {
					return nil, err
				}
				baseType, baseData = br.typ, br.data
			} else {
				if br, ok := byID[e.baseID]; ok {
					baseType, baseData = br.typ, br.data
				} else {
					for _, cand := range entries {
						if cand == e || cand.typ == "" {
							continue
						}
						cr, err := resolve(cand, depth+1)
						if err != nil {
							return nil, err
						}
						if cr.id == e.baseID {
							baseType, baseData = cr.typ, cr.data
							break
						}
					}
					if baseData == nil {
						if obj := lookup(e.baseID); obj != nil {
							baseType, baseData = obj.Type, obj.Data
						}
					}
					if baseData == nil {
						return nil, fmt.Errorf("ref-delta base %s is missing", e.baseID)
					}
				}
			}
			data, err := applyDelta(baseData, e.data)
			if err != nil {
				return nil, err
			}
			r = &result{typ: baseType, data: data, id: hashObject(baseType, data)}
		}
		resolved[e] = r
		byID[r.id] = r
		return r, nil
	}
	out := make([]ParsedObject, 0, len(entries))
	for _, e := range entries {
		r, err := resolve(e, 0)
		if err != nil {
			return nil, err
		}
		out = append(out, ParsedObject{ID: r.id, Type: r.typ, Data: r.data})
	}
	return out, nil
}

// ---------------------------------------------------------------- writing

var deflatedCache = map[string][]byte{}
var deflatedBytes int

const deflateCacheLimit = 256 * 1024 * 1024

func entryHeader(typ string, size int) []byte {
	var out []byte
	b := byte(codeByType[typ]<<4) | byte(size&0x0f)
	rest := size >> 4
	for rest > 0 {
		out = append(out, b|0x80)
		b = byte(rest & 0x7f)
		rest >>= 7
	}
	return append(out, b)
}

func packEntry(id string, obj *GitObject) []byte {
	if cached, ok := deflatedCache[id]; ok {
		return cached
	}
	entry := append(entryHeader(obj.Type, len(obj.Data)), deflate(obj.Data)...)
	if deflatedBytes+len(entry) <= deflateCacheLimit {
		deflatedCache[id] = entry
		deflatedBytes += len(entry)
	}
	return entry
}

// writePack writes a version-2 pack of whole objects, in the given order.
func writePack(ids []string, get func(string) *GitObject) ([]byte, error) {
	var b bytes.Buffer
	header := make([]byte, 12)
	copy(header, "PACK")
	binary.BigEndian.PutUint32(header[4:], 2)
	binary.BigEndian.PutUint32(header[8:], uint32(len(ids)))
	b.Write(header)
	for _, id := range ids {
		obj := get(id)
		if obj == nil {
			return nil, fmt.Errorf("object %s is missing from the repository", id)
		}
		b.Write(packEntry(id, obj))
	}
	sum := sha1.Sum(b.Bytes())
	b.Write(sum[:])
	return b.Bytes(), nil
}
