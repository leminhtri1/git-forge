package main

// Git smart HTTP (the Go twin of src/git/smart-http.ts): info/refs, git-upload-pack (protocol v2 with a
// v0 fallback) and git-receive-pack (v0, report-status, side-band-64k, delete-refs).

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const agentCap = "agent=git-forge/1.0"

var gitPathRE = regexp.MustCompile(`^/([^/]+)/([^/]+?)(?:\.git)?/(info/refs|git-upload-pack|git-receive-pack)$`)

// ---------------------------------------------------------------- pkt-lines

const maxPktPayload = 65516

var flushPkt = []byte("0000")
var delimPkt = []byte("0001")

func pkt(b *bytes.Buffer, payload string) {
	fmt.Fprintf(b, "%04x", len(payload)+4)
	b.WriteString(payload)
}

func sideband(b *bytes.Buffer, band byte, data []byte) {
	chunk := maxPktPayload - 1
	for at := 0; at < len(data); at += chunk {
		end := at + chunk
		if end > len(data) {
			end = len(data)
		}
		fmt.Fprintf(b, "%04x", end-at+5)
		b.WriteByte(band)
		b.Write(data[at:end])
	}
}

type packet struct {
	kind string // data, flush, delim, end
	data []byte
}

func (p packet) line() string {
	return strings.TrimSuffix(string(p.data), "\n")
}

func decodePackets(buf []byte, stopAtFlush bool) ([]packet, int, error) {
	var packets []packet
	at := 0
	for at < len(buf) {
		if len(buf)-at < 4 {
			return nil, 0, fmt.Errorf("truncated pkt-line length")
		}
		n, err := strconv.ParseUint(string(buf[at:at+4]), 16, 32)
		if err != nil {
			return nil, 0, fmt.Errorf("bad pkt-line length %q", buf[at:at+4])
		}
		switch n {
		case 0:
			packets = append(packets, packet{kind: "flush"})
			at += 4
			if stopAtFlush {
				return packets, at, nil
			}
			continue
		case 1:
			packets = append(packets, packet{kind: "delim"})
			at += 4
			continue
		case 2:
			packets = append(packets, packet{kind: "end"})
			at += 4
			continue
		}
		if n < 4 {
			return nil, 0, fmt.Errorf("invalid pkt-line length %d", n)
		}
		if at+int(n) > len(buf) {
			return nil, 0, fmt.Errorf("truncated pkt-line payload")
		}
		packets = append(packets, packet{kind: "data", data: buf[at+4 : at+int(n)]})
		at += int(n)
	}
	return packets, at, nil
}

// ---------------------------------------------------------------- responses

func gitResp(status int, contentType string, body []byte) *Response {
	return &Response{Status: status, Body: body, Headers: [][2]string{
		{"content-type", contentType},
		{"cache-control", "no-cache, max-age=0, must-revalidate"},
		{"expires", "Fri, 01 Jan 1980 00:00:00 GMT"},
		{"pragma", "no-cache"},
		{"x-github-request-id", nextRequestID()},
	}}
}

func textResp(status int, text string, extra ...[2]string) *Response {
	resp := &Response{Status: status, Body: []byte(text + "\n"), Headers: [][2]string{{"content-type", "text/plain; charset=utf-8"}, {"x-github-request-id", nextRequestID()}}}
	resp.Headers = append(resp.Headers, extra...)
	return resp
}

func wantsV2(r *Request) bool {
	for _, part := range strings.Split(r.Headers["git-protocol"], ":") {
		if strings.TrimSpace(part) == "version=2" {
			return true
		}
	}
	return false
}

func isGitPath(path string) bool {
	return strings.HasSuffix(path, "/info/refs") || strings.HasSuffix(path, "/git-upload-pack") || strings.HasSuffix(path, "/git-receive-pack")
}

// handleGit answers a git smart-HTTP request; nil when the path is not a git path.
func handleGit(r *Request) *Response {
	m := gitPathRE.FindStringSubmatch(r.Path)
	if m == nil {
		return nil
	}
	owner, err1 := url.PathUnescape(m[1])
	name, err2 := url.PathUnescape(m[2])
	if err1 != nil || err2 != nil {
		return nil
	}
	action := m[3]
	if !isAuthorized(r) {
		return textResp(401, "Authentication required", [2]string{"www-authenticate", `Basic realm="git-forge"`})
	}
	repo := lookupRepo(owner, name)
	if repo == nil {
		return textResp(404, "Repository not found")
	}
	if action == "info/refs" {
		if r.Method != "GET" && r.Method != "HEAD" {
			return textResp(405, "Method Not Allowed")
		}
		service := r.Query().Get("service")
		if service != "git-upload-pack" && service != "git-receive-pack" {
			return textResp(403, "dumb HTTP is not served; use a smart git client")
		}
		var body []byte
		if service == "git-upload-pack" && wantsV2(r) {
			body = v2Advertisement
		} else {
			body = advertiseV0(repo, service)
		}
		return gitResp(200, "application/x-"+service+"-advertisement", body)
	}
	if r.Method != "POST" {
		return textResp(405, "Method Not Allowed")
	}
	raw, err := decodeBody(r)
	if err != nil {
		return textResp(400, "bad request body: "+err.Error())
	}
	var out []byte
	if action == "git-upload-pack" {
		v2 := wantsV2(r) || (len(raw) >= 12 && string(raw[4:12]) == "command=")
		if v2 {
			out, err = uploadPackV2(repo, raw)
		} else {
			out, err = uploadPackV0(repo, raw)
		}
		if err == nil {
			return gitResp(200, "application/x-git-upload-pack-result", out)
		}
	} else {
		out, err = receivePack(repo, raw)
		if err == nil {
			return gitResp(200, "application/x-git-receive-pack-result", out)
		}
	}
	return textResp(400, "bad "+action+" request: "+err.Error())
}

// decodeBody undoes a gzip or deflate content-encoding (real git gzips large bodies).
func decodeBody(r *Request) ([]byte, error) {
	switch strings.ToLower(r.Headers["content-encoding"]) {
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(bytes.NewReader(r.Body))
		if err != nil {
			return nil, err
		}
		return io.ReadAll(zr)
	case "deflate":
		zr, err := zlib.NewReader(bytes.NewReader(r.Body))
		if err != nil {
			return io.ReadAll(flate.NewReader(bytes.NewReader(r.Body)))
		}
		return io.ReadAll(zr)
	}
	return r.Body, nil
}

// ---------------------------------------------------------------- advertisements

var v2Advertisement = func() []byte {
	var b bytes.Buffer
	pkt(&b, "version 2\n")
	pkt(&b, agentCap+"\n")
	pkt(&b, "ls-refs=unborn\n")
	pkt(&b, "fetch=shallow\n")
	pkt(&b, "server-option\n")
	pkt(&b, "object-format=sha1\n")
	b.Write(flushPkt)
	return b.Bytes()
}()

var advertisementCache = map[string][]byte{}

func advertiseV0(repo *Repo, service string) []byte {
	key := fmt.Sprintf("%d|%d|%s|%s", repo.ID, repo.RefsVersion, repo.DefaultBranch, service)
	if cached, ok := advertisementCache[key]; ok {
		return cached
	}
	var b bytes.Buffer
	pkt(&b, "# service="+service+"\n")
	b.Write(flushPkt)
	var caps []string
	if service == "git-upload-pack" {
		caps = []string{"multi_ack_detailed", "side-band-64k", "thin-pack", "ofs-delta", "shallow", "no-progress", "include-tag", "allow-tip-sha1-in-want", "allow-reachable-sha1-in-want", "symref=HEAD:" + repo.HeadRefName(), "object-format=sha1", agentCap}
	} else {
		caps = []string{"report-status", "delete-refs", "side-band-64k", "quiet", "ofs-delta", "object-format=sha1", agentCap}
	}
	var lines []string
	if service == "git-upload-pack" {
		if head, ok := repo.Refs[repo.HeadRefName()]; ok {
			lines = append(lines, head+" HEAD")
		}
	}
	for _, ref := range repo.SortedRefs() {
		lines = append(lines, ref[1]+" "+ref[0])
		if service == "git-upload-pack" && strings.HasPrefix(ref[0], "refs/tags/") {
			if peeled := repo.Peel(ref[1]); peeled != ref[1] {
				lines = append(lines, peeled+" "+ref[0]+"^{}")
			}
		}
	}
	if len(lines) == 0 {
		lines = append(lines, zeroID+" capabilities^{}")
	}
	for i, line := range lines {
		if i == 0 {
			pkt(&b, line+"\x00"+strings.Join(caps, " ")+"\n")
		} else {
			pkt(&b, line+"\n")
		}
	}
	b.Write(flushPkt)
	if len(advertisementCache) > 4096 {
		advertisementCache = map[string][]byte{}
	}
	advertisementCache[key] = b.Bytes()
	return b.Bytes()
}

// ---------------------------------------------------------------- upload-pack v2

func splitCommand(packets []packet) (string, []string) {
	command := ""
	var args []string
	inArgs := false
	for _, p := range packets {
		if p.kind == "delim" {
			inArgs = true
			continue
		}
		if p.kind == "flush" || p.kind == "end" {
			break
		}
		line := p.line()
		if !inArgs {
			if strings.HasPrefix(line, "command=") {
				command = line[8:]
			}
		} else {
			args = append(args, line)
		}
	}
	return command, args
}

func uploadPackV2(repo *Repo, raw []byte) ([]byte, error) {
	packets, _, err := decodePackets(raw, false)
	if err != nil {
		return nil, err
	}
	command, args := splitCommand(packets)
	switch command {
	case "ls-refs":
		return lsRefs(repo, args), nil
	case "fetch":
		return fetchV2(repo, args)
	}
	return nil, fmt.Errorf("unknown command %q", command)
}

var lsRefsCache = map[string][]byte{}

func lsRefs(repo *Repo, args []string) []byte {
	key := fmt.Sprintf("%d|%d|%s|%s", repo.ID, repo.RefsVersion, repo.DefaultBranch, strings.Join(args, "\n"))
	if cached, ok := lsRefsCache[key]; ok {
		return cached
	}
	has := func(a string) bool {
		for _, x := range args {
			if x == a {
				return true
			}
		}
		return false
	}
	peel, symrefs, unborn := has("peel"), has("symrefs"), has("unborn")
	var prefixes []string
	for _, a := range args {
		if strings.HasPrefix(a, "ref-prefix ") {
			prefixes = append(prefixes, a[11:])
		}
	}
	wanted := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	var b bytes.Buffer
	if wanted("HEAD") {
		target := ""
		if symrefs {
			target = " symref-target:" + repo.HeadRefName()
		}
		if head, ok := repo.Refs[repo.HeadRefName()]; ok {
			pkt(&b, head+" HEAD"+target+"\n")
		} else if unborn {
			pkt(&b, "unborn HEAD"+target+"\n")
		}
	}
	for _, ref := range repo.SortedRefs() {
		if !wanted(ref[0]) {
			continue
		}
		line := ref[1] + " " + ref[0]
		if peel {
			if peeled := repo.Peel(ref[1]); peeled != ref[1] {
				line += " peeled:" + peeled
			}
		}
		pkt(&b, line+"\n")
	}
	b.Write(flushPkt)
	if len(lsRefsCache) > 4096 {
		lsRefsCache = map[string][]byte{}
	}
	lsRefsCache[key] = b.Bytes()
	return b.Bytes()
}

type fetchArgs struct {
	wants, haves, shallow []string
	depth                 int // 0: none
	done, includeTag      bool
	sideband              bool
}

func parseFetchArgs(args []string) fetchArgs {
	f := fetchArgs{sideband: true}
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "want "):
			f.wants = append(f.wants, prefix40(a[5:]))
		case strings.HasPrefix(a, "have "):
			f.haves = append(f.haves, prefix40(a[5:]))
		case strings.HasPrefix(a, "shallow "):
			f.shallow = append(f.shallow, prefix40(a[8:]))
		case strings.HasPrefix(a, "deepen "):
			n, _ := strconv.Atoi(strings.TrimSpace(a[7:]))
			if n < 1 {
				n = 1
			}
			f.depth = n
		case a == "done":
			f.done = true
		case a == "include-tag":
			f.includeTag = true
		case strings.HasPrefix(a, "want-ref "):
			f.wants = append(f.wants, "ref:"+a[9:])
		}
	}
	return f
}

func prefix40(s string) string {
	if len(s) > 40 {
		return s[:40]
	}
	return s
}

type packPlan struct {
	ids, shallow, unshallow, common []string
}

// planFetch decides which objects a fetch sends (see the TypeScript twin for the reasoning).
func planFetch(repo *Repo, req fetchArgs) packPlan {
	clientShallow := map[string]bool{}
	for _, id := range req.shallow {
		if repo.Get(id) != nil {
			clientShallow[id] = true
		}
	}
	var common []string
	for _, id := range req.haves {
		if repo.Get(id) != nil {
			common = append(common, id)
		}
	}
	haveCommits := map[string]bool{}
	haveObjects := map[string]bool{}
	var stack []string
	for _, have := range common {
		peeled := repo.Peel(have)
		if peeled != have {
			haveObjects[have] = true
		}
		stack = append(stack, peeled)
	}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if haveCommits[id] {
			continue
		}
		c := repo.Commit(id)
		if c == nil {
			haveObjects[id] = true
			continue
		}
		haveCommits[id] = true
		markTree(repo, c.Tree, haveObjects)
		if !clientShallow[id] {
			stack = append(stack, c.Parents...)
		}
	}

	var send []string
	sent := map[string]bool{}
	add := func(id string) {
		if !sent[id] && !haveObjects[id] && !haveCommits[id] {
			sent[id] = true
			send = append(send, id)
		}
	}
	var wantCommits []string
	for _, want := range req.wants {
		id := want
		for {
			obj := repo.Get(id)
			if obj == nil || obj.Type != "tag" {
				break
			}
			add(id)
			id = parseTag(obj.Data).Object
		}
		obj := repo.Get(id)
		if obj == nil {
			continue
		}
		switch obj.Type {
		case "commit":
			wantCommits = append(wantCommits, id)
		case "tree":
			addTree(repo, id, add, haveObjects)
		default:
			add(id)
		}
	}

	var shallow, unshallow, commits []string
	if req.depth > 0 {
		depthOf := map[string]int{}
		var frontier []string
		for _, id := range wantCommits {
			if _, seen := depthOf[id]; !seen {
				depthOf[id] = 1
				frontier = append(frontier, id)
			}
		}
		for level := 1; len(frontier) > 0 && level <= req.depth; level++ {
			var next []string
			for _, id := range frontier {
				c := repo.Commit(id)
				if c == nil {
					continue
				}
				commits = append(commits, id)
				if level == req.depth {
					if len(c.Parents) > 0 && !clientShallow[id] {
						shallow = append(shallow, id)
					}
					continue
				}
				if clientShallow[id] && len(c.Parents) > 0 {
					unshallow = append(unshallow, id)
				}
				for _, p := range c.Parents {
					if _, seen := depthOf[p]; !seen {
						depthOf[p] = level + 1
						next = append(next, p)
					}
				}
			}
			frontier = next
		}
	} else {
		seen := map[string]bool{}
		walk := append([]string(nil), wantCommits...)
		for len(walk) > 0 {
			id := walk[len(walk)-1]
			walk = walk[:len(walk)-1]
			if seen[id] || haveCommits[id] {
				continue
			}
			seen[id] = true
			c := repo.Commit(id)
			if c == nil {
				continue
			}
			commits = append(commits, id)
			walk = append(walk, c.Parents...)
		}
	}
	isUnshallow := map[string]bool{}
	for _, id := range unshallow {
		isUnshallow[id] = true
	}
	for _, id := range commits {
		if haveCommits[id] && !isUnshallow[id] {
			continue
		}
		c := repo.Commit(id)
		if !haveCommits[id] {
			add(id)
		}
		addTree(repo, c.Tree, add, haveObjects)
	}
	if req.includeTag {
		for _, ref := range repo.SortedRefs() {
			if !strings.HasPrefix(ref[0], "refs/tags/") || sent[ref[1]] {
				continue
			}
			if sent[repo.Peel(ref[1])] {
				if obj := repo.Get(ref[1]); obj != nil && obj.Type == "tag" {
					add(ref[1])
				}
			}
		}
	}
	return packPlan{ids: send, shallow: shallow, unshallow: unshallow, common: common}
}

func markTree(repo *Repo, treeID string, into map[string]bool) {
	if into[treeID] {
		return
	}
	into[treeID] = true
	obj := repo.Get(treeID)
	if obj == nil || obj.Type != "tree" {
		return
	}
	entries, _ := parseTree(obj.Data)
	for _, e := range entries {
		if e.Mode == "160000" {
			continue
		}
		if isTreeMode(e.Mode) {
			markTree(repo, e.ID, into)
		} else {
			into[e.ID] = true
		}
	}
}

func addTree(repo *Repo, treeID string, add func(string), have map[string]bool) {
	var visit func(id string)
	visit = func(id string) {
		if have[id] {
			return
		}
		obj := repo.Get(id)
		if obj == nil {
			return
		}
		add(id)
		if obj.Type != "tree" {
			return
		}
		entries, _ := parseTree(obj.Data)
		for _, e := range entries {
			if e.Mode == "160000" {
				continue
			}
			if isTreeMode(e.Mode) {
				visit(e.ID)
			} else if !have[e.ID] && repo.Get(e.ID) != nil {
				add(e.ID)
			}
		}
	}
	visit(treeID)
}

var fetchCache = map[string][]byte{}
var fetchCacheBytes int

const fetchCacheLimit = 128 * 1024 * 1024

func sortedCopy(s []string) string {
	c := append([]string(nil), s...)
	sort.Strings(c)
	return strings.Join(c, ",")
}

func fetchV2(repo *Repo, args []string) ([]byte, error) {
	req := parseFetchArgs(args)
	var wants []string
	for _, w := range req.wants {
		if strings.HasPrefix(w, "ref:") {
			w = repo.Refs[w[4:]]
		}
		if isObjectID(w) {
			wants = append(wants, w)
		}
	}
	req.wants = wants
	done, include := 0, 0
	if req.done {
		done = 1
	}
	if req.includeTag {
		include = 1
	}
	key := fmt.Sprintf("v2|%d|%d|%d|%d|%d|%d|%s|%s|%s", repo.ID, repo.RefsVersion, len(repo.Objects), done, include, req.depth, sortedCopy(req.wants), sortedCopy(req.haves), sortedCopy(req.shallow))
	if cached, ok := fetchCache[key]; ok {
		return cached, nil
	}
	for _, w := range req.wants {
		if repo.Get(w) == nil {
			return nil, fmt.Errorf("not our ref %s", w)
		}
	}
	plan := planFetch(repo, req)
	var b bytes.Buffer
	if !req.done {
		pkt(&b, "acknowledgments\n")
		if len(plan.common) == 0 {
			pkt(&b, "NAK\n")
		}
		for _, id := range plan.common {
			pkt(&b, "ACK "+id+"\n")
		}
		pkt(&b, "ready\n")
		b.Write(delimPkt)
	}
	if len(plan.shallow) > 0 || len(plan.unshallow) > 0 {
		pkt(&b, "shallow-info\n")
		for _, id := range plan.shallow {
			pkt(&b, "shallow "+id+"\n")
		}
		for _, id := range plan.unshallow {
			pkt(&b, "unshallow "+id+"\n")
		}
		b.Write(delimPkt)
	}
	pkt(&b, "packfile\n")
	pack, err := writePack(plan.ids, repo.Get)
	if err != nil {
		return nil, err
	}
	sideband(&b, 1, pack)
	b.Write(flushPkt)
	body := b.Bytes()
	if fetchCacheBytes+len(body) > fetchCacheLimit {
		fetchCache = map[string][]byte{}
		fetchCacheBytes = 0
	}
	fetchCache[key] = body
	fetchCacheBytes += len(body)
	return body, nil
}

// ---------------------------------------------------------------- upload-pack v0

func uploadPackV0(repo *Repo, raw []byte) ([]byte, error) {
	packets, _, err := decodePackets(raw, false)
	if err != nil {
		return nil, err
	}
	req := fetchArgs{}
	for _, p := range packets {
		if p.kind != "data" {
			continue
		}
		line := p.line()
		switch {
		case strings.HasPrefix(line, "want "):
			req.wants = append(req.wants, prefix40(line[5:]))
			if len(line) > 46 {
				for _, c := range strings.Split(line[46:], " ") {
					if c == "side-band-64k" || c == "side-band" {
						req.sideband = true
					}
					if c == "include-tag" {
						req.includeTag = true
					}
				}
			}
		case strings.HasPrefix(line, "have "):
			req.haves = append(req.haves, prefix40(line[5:]))
		case strings.HasPrefix(line, "shallow "):
			req.shallow = append(req.shallow, prefix40(line[8:]))
		case strings.HasPrefix(line, "deepen "):
			n, _ := strconv.Atoi(strings.TrimSpace(line[7:]))
			if n < 1 {
				n = 1
			}
			req.depth = n
		case line == "done":
			req.done = true
		}
	}
	if len(req.wants) == 0 {
		return []byte{}, nil
	}
	for _, w := range req.wants {
		if repo.Get(w) == nil {
			return nil, fmt.Errorf("not our ref %s", w)
		}
	}
	plan := planFetch(repo, req)
	var b bytes.Buffer
	if req.depth > 0 || len(req.shallow) > 0 {
		for _, id := range plan.shallow {
			pkt(&b, "shallow "+id+"\n")
		}
		for _, id := range plan.unshallow {
			pkt(&b, "unshallow "+id+"\n")
		}
		b.Write(flushPkt)
	}
	if !req.done && len(req.haves) > 0 && len(plan.common) > 0 {
		for _, id := range plan.common {
			pkt(&b, "ACK "+id+" common\n")
		}
		pkt(&b, "ACK "+plan.common[len(plan.common)-1]+" ready\n")
	}
	if len(plan.common) > 0 {
		pkt(&b, "ACK "+plan.common[len(plan.common)-1]+"\n")
	} else {
		pkt(&b, "NAK\n")
	}
	pack, err := writePack(plan.ids, repo.Get)
	if err != nil {
		return nil, err
	}
	if req.sideband {
		sideband(&b, 1, pack)
		b.Write(flushPkt)
	} else {
		b.Write(pack)
	}
	return b.Bytes(), nil
}

// ---------------------------------------------------------------- receive-pack

type refCommand struct{ oldID, newID, ref string }

var funnyRefRE = regexp.MustCompile(`[\s~^:?*\[\\]`)

func receivePack(repo *Repo, raw []byte) ([]byte, error) {
	packets, end, err := decodePackets(raw, true)
	if err != nil {
		return nil, err
	}
	var commands []refCommand
	var caps []string
	for _, p := range packets {
		if p.kind != "data" {
			continue
		}
		line := strings.TrimSuffix(string(p.data), "\n")
		if nul := strings.IndexByte(line, 0); nul >= 0 {
			caps = strings.Fields(line[nul+1:])
			line = line[:nul]
		}
		if strings.HasPrefix(line, "shallow ") {
			continue
		}
		parts := strings.Split(line, " ")
		if len(parts) < 3 || !isObjectID(parts[0]) || !isObjectID(parts[1]) || parts[2] == "" {
			return nil, fmt.Errorf("bad command %q", line)
		}
		commands = append(commands, refCommand{parts[0], parts[1], parts[2]})
	}
	hasCap := func(c string) bool {
		for _, x := range caps {
			if x == c {
				return true
			}
		}
		return false
	}
	useSideband := hasCap("side-band-64k") || hasCap("side-band")
	reportStatus := hasCap("report-status") || hasCap("report-status-v2")

	unpackError := ""
	if packData := raw[end:]; len(packData) > 0 {
		objects, err := readPack(packData, repo.Get)
		if err != nil {
			unpackError = err.Error()
		} else {
			for _, o := range objects {
				if repo.Objects[o.ID] == nil {
					repo.Objects[o.ID] = &GitObject{Type: o.Type, Data: o.Data}
				}
			}
		}
	}
	hadBranches := !repo.IsEmpty()
	results := make([]string, 0, len(commands))
	for _, c := range commands {
		if unpackError != "" {
			results = append(results, "ng "+c.ref+" unpacker error")
			continue
		}
		if reason := applyCommand(repo, c); reason == "" {
			results = append(results, "ok "+c.ref)
		} else {
			results = append(results, "ng "+c.ref+" "+reason)
		}
	}
	if _, hasHead := repo.Refs[repo.HeadRefName()]; !hadBranches && !hasHead {
		for _, c := range commands {
			if _, exists := repo.Refs[c.ref]; strings.HasPrefix(c.ref, "refs/heads/") && exists {
				repo.DefaultBranch = c.ref[len("refs/heads/"):]
				break
			}
		}
	}
	if !reportStatus {
		return []byte{}, nil
	}
	var report bytes.Buffer
	if unpackError == "" {
		pkt(&report, "unpack ok\n")
	} else {
		pkt(&report, "unpack "+strings.ReplaceAll(unpackError, "\n", " ")+"\n")
	}
	for _, line := range results {
		pkt(&report, line+"\n")
	}
	report.Write(flushPkt)
	if !useSideband {
		return report.Bytes(), nil
	}
	var out bytes.Buffer
	sideband(&out, 1, report.Bytes())
	out.Write(flushPkt)
	return out.Bytes(), nil
}

// applyCommand applies one ref update; "" on success, else the ng reason.
func applyCommand(repo *Repo, c refCommand) string {
	if !strings.HasPrefix(c.ref, "refs/") || strings.Contains(c.ref, "..") || funnyRefRE.MatchString(c.ref) || strings.HasSuffix(c.ref, "/") || strings.HasSuffix(c.ref, ".lock") {
		return "funny refname"
	}
	current, ok := repo.Refs[c.ref]
	if !ok {
		current = zeroID
	}
	if current != c.oldID {
		if c.oldID == zeroID && current != zeroID {
			return "already exists"
		}
		return "stale info"
	}
	if c.newID == zeroID {
		if current == zeroID {
			return "no such ref"
		}
		if c.ref == repo.HeadRefName() {
			for name := range repo.Refs {
				if strings.HasPrefix(name, "refs/heads/") && name != c.ref {
					return "refusing to delete the current branch"
				}
			}
		}
		repo.SetRef(c.ref, "")
		return ""
	}
	obj := repo.Get(c.newID)
	if obj == nil {
		return "missing necessary objects"
	}
	if strings.HasPrefix(c.ref, "refs/heads/") && obj.Type != "commit" {
		return "not a commit"
	}
	if current != zeroID {
		oldCommit := repo.Peel(current)
		newCommit := repo.Peel(c.newID)
		oc, nc := repo.Get(oldCommit), repo.Get(newCommit)
		if oc == nil || nc == nil || oc.Type != "commit" || nc.Type != "commit" || !repo.IsAncestor(oldCommit, newCommit) {
			return "non-fast-forward"
		}
	} else if !hasClosure(repo, c.newID) {
		return "missing necessary objects"
	}
	repo.SetRef(c.ref, c.newID)
	return ""
}

func hasClosure(repo *Repo, id string) bool {
	peeled := repo.Peel(id)
	obj := repo.Get(peeled)
	if obj == nil {
		return false
	}
	if obj.Type != "commit" {
		return true
	}
	c := repo.Commit(peeled)
	if repo.Get(c.Tree) == nil {
		return false
	}
	for _, p := range c.Parents {
		if repo.Get(p) == nil {
			return false
		}
	}
	return true
}
