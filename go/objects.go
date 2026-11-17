package main

// Git's object model: loose-object hashing and the commit/tree/tag formats, built and parsed by hand.

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type GitObject struct {
	Type string // commit, tree, blob, tag
	Data []byte
}

const zeroID = "0000000000000000000000000000000000000000"

var objectIDRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

func isObjectID(s string) bool { return len(s) == 40 && objectIDRE.MatchString(s) }

func hashObject(typ string, data []byte) string {
	h := sha1.New()
	h.Write([]byte(typ))
	h.Write([]byte{' '})
	h.Write([]byte(strconv.Itoa(len(data))))
	h.Write([]byte{0})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// ---------------------------------------------------------------- trees

type TreeEntry struct {
	Mode string
	Name string
	ID   string
}

func isTreeMode(mode string) bool { return mode == "40000" || mode == "040000" }

func parseTree(data []byte) ([]TreeEntry, error) {
	var entries []TreeEntry
	at := 0
	for at < len(data) {
		space := bytes.IndexByte(data[at:], ' ')
		if space < 0 {
			return nil, fmt.Errorf("malformed tree object")
		}
		space += at
		nul := bytes.IndexByte(data[space+1:], 0)
		if nul < 0 {
			return nil, fmt.Errorf("malformed tree object")
		}
		nul += space + 1
		if nul+21 > len(data) {
			return nil, fmt.Errorf("malformed tree object")
		}
		entries = append(entries, TreeEntry{Mode: string(data[at:space]), Name: string(data[space+1 : nul]), ID: hex.EncodeToString(data[nul+1 : nul+21])})
		at = nul + 21
	}
	return entries, nil
}

// buildTree writes entries in git's order: names compared bytewise, directories as if ending in '/'.
func buildTree(entries []TreeEntry) []byte {
	sorted := append([]TreeEntry(nil), entries...)
	key := func(e TreeEntry) string {
		if isTreeMode(e.Mode) {
			return e.Name + "/"
		}
		return e.Name
	}
	sort.Slice(sorted, func(i, j int) bool { return key(sorted[i]) < key(sorted[j]) })
	var b bytes.Buffer
	for _, e := range sorted {
		mode := e.Mode
		if isTreeMode(mode) {
			mode = "40000"
		}
		b.WriteString(mode)
		b.WriteByte(' ')
		b.WriteString(e.Name)
		b.WriteByte(0)
		raw, _ := hex.DecodeString(e.ID)
		b.Write(raw)
	}
	return b.Bytes()
}

// ---------------------------------------------------------------- commits

type Commit struct {
	Tree      string
	Parents   []string
	Author    string // "Name <email> 1700000000 +0000"
	Committer string
	Message   string
}

func parseCommit(data []byte) Commit {
	text := string(data)
	headText, message := text, ""
	if split := strings.Index(text, "\n\n"); split >= 0 {
		headText, message = text[:split], text[split+2:]
	}
	c := Commit{Message: message}
	for _, line := range strings.Split(headText, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			c.Tree = line[5:]
		case strings.HasPrefix(line, "parent "):
			c.Parents = append(c.Parents, line[7:])
		case strings.HasPrefix(line, "author "):
			c.Author = line[7:]
		case strings.HasPrefix(line, "committer "):
			c.Committer = line[10:]
		}
	}
	return c
}

func buildCommit(c Commit) []byte {
	var b strings.Builder
	b.WriteString("tree " + c.Tree + "\n")
	for _, p := range c.Parents {
		b.WriteString("parent " + p + "\n")
	}
	b.WriteString("author " + c.Author + "\ncommitter " + c.Committer + "\n\n" + c.Message)
	return []byte(b.String())
}

type Signature struct {
	Name, Email, TZ string
	Time            int64
}

var signatureRE = regexp.MustCompile(`^(.*?) <([^>]*)> (-?\d+) ([+-]\d{4})$`)

func parseSignature(sig string) Signature {
	m := signatureRE.FindStringSubmatch(sig)
	if m == nil {
		return Signature{Name: sig, TZ: "+0000"}
	}
	t, _ := strconv.ParseInt(m[3], 10, 64)
	return Signature{Name: m[1], Email: m[2], Time: t, TZ: m[4]}
}

func makeSignature(name, email string, t int64) string {
	return fmt.Sprintf("%s <%s> %d +0000", name, email, t)
}

// signatureDate is an ISO timestamp honouring the signature's timezone offset.
func signatureDate(sig string) string {
	s := parseSignature(sig)
	sign := 1
	if strings.HasPrefix(s.TZ, "-") {
		sign = -1
	}
	hours, _ := strconv.Atoi(s.TZ[1:3])
	minutes, _ := strconv.Atoi(s.TZ[3:5])
	offset := sign * (hours*60 + minutes)
	local := time.Unix(s.Time+int64(offset*60), 0).UTC().Format("2006-01-02T15:04:05")
	if offset == 0 {
		return local + "Z"
	}
	return local + s.TZ[:3] + ":" + s.TZ[3:]
}

// ---------------------------------------------------------------- tags

type Tag struct {
	Object, Type, Tag string
}

func parseTag(data []byte) Tag {
	text := string(data)
	if split := strings.Index(text, "\n\n"); split >= 0 {
		text = text[:split]
	}
	var t Tag
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "object "):
			t.Object = line[7:]
		case strings.HasPrefix(line, "type "):
			t.Type = line[5:]
		case strings.HasPrefix(line, "tag "):
			t.Tag = line[4:]
		}
	}
	return t
}
