package main

// Domain operations shared by REST, GraphQL and MCP (the Go twin of src/ops.ts). They validate the way
// GitHub's REST API does and fail by panicking with *HTTPError carrying GitHub's status; each surface
// recovers that into its own error shape.

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var repoNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
var colorRE = regexp.MustCompile(`^#?[0-9a-fA-F]{6}$`)

func fail(status int, message string) { panic(httpErr(status, message)) }

func validation(message, resource, field, code string) *HTTPError {
	return &HTTPError{Status: 422, Message: message, Extra: Obj{{"errors", []any{Obj{{"resource", resource}, {"field", field}, {"code", code}, {"message", message}}}}}}
}

func customError(message string) *HTTPError {
	return &HTTPError{Status: 422, Message: "Validation Failed", Extra: Obj{{"errors", []any{Obj{{"resource", "PullRequest"}, {"code", "custom"}, {"message", message}}}}}}
}

func optString(m map[string]any, key string) *string {
	if s, ok := m[key].(string); ok {
		return &s
	}
	return nil
}

// ---------------------------------------------------------------- repositories

func requireRepo(owner, name string) *Repo {
	r := lookupRepo(owner, name)
	if r == nil {
		fail(404, "Not Found")
	}
	return r
}

func createRepository(in map[string]any) *Repo {
	name, _ := in["name"].(string)
	name = strings.TrimSpace(name)
	if name == "" || !repoNameRE.MatchString(name) || name == "." || name == ".." {
		code := "invalid"
		if name == "" {
			code = "missing_field"
		}
		panic(validation("Repository creation failed.", "Repository", "name", code))
	}
	if store.Find(config.Viewer.Login, name) != nil {
		panic(&HTTPError{Status: 422, Message: "Repository creation failed.", Extra: Obj{{"errors", []any{Obj{{"resource", "Repository"}, {"code", "custom"}, {"field", "name"}, {"message", "name already exists on this account"}}}}}})
	}
	visibility, _ := in["visibility"].(string)
	isPrivate := in["private"] == true || visibility == "private" || visibility == "internal"
	r := store.Create(config.Viewer.Login, name, optString(in, "description"), isPrivate)
	if hp := optString(in, "homepage"); hp != nil {
		r.Homepage = hp
	}
	if in["auto_init"] == true {
		text := "# " + name + "\n"
		if r.Description != nil && *r.Description != "" {
			text += "\n" + *r.Description + "\n"
		}
		blob := r.Put("blob", []byte(text))
		tree := r.WriteFlat(map[string]FlatEntry{"README.md": {Mode: "100644", ID: blob}})
		r.SetRef(r.HeadRefName(), r.MakeCommit(tree, nil, "Initial commit"))
	}
	return r
}

func updateRepository(r *Repo, in map[string]any) *Repo {
	if v, ok := in["name"]; ok {
		name, isStr := v.(string)
		if !isStr || !repoNameRE.MatchString(name) {
			panic(validation("Validation Failed", "Repository", "name", "invalid"))
		}
		if !strings.EqualFold(name, r.Name) && store.Find(r.Owner, name) != nil {
			panic(validation("name already exists on this account", "Repository", "name", "custom"))
		}
		store.Rename(r, name)
	}
	if v, ok := in["description"]; ok {
		if s, isStr := v.(string); isStr {
			r.Description = &s
		} else {
			r.Description = nil
		}
	}
	if v, ok := in["homepage"]; ok {
		if s, isStr := v.(string); isStr {
			r.Homepage = &s
		} else {
			r.Homepage = nil
		}
	}
	if v, ok := in["private"]; ok {
		r.IsPrivate = v == true
	}
	if v, ok := in["visibility"]; ok && v != nil {
		r.IsPrivate = v != "public"
	}
	if db, ok := in["default_branch"].(string); ok {
		if _, exists := r.BranchTip(db); !r.IsEmpty() && !exists {
			panic(validation("Validation Failed", "Repository", "default_branch", "invalid"))
		}
		r.DefaultBranch = db
	}
	_, r.UpdatedAt = tick()
	return r
}

// ---------------------------------------------------------------- labels

func createLabel(r *Repo, in map[string]any) *Label {
	name, _ := in["name"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		panic(validation("Validation Failed", "Label", "name", "missing_field"))
	}
	if _, ok := r.Labels[strings.ToLower(name)]; ok {
		panic(validation("Validation Failed", "Label", "name", "already_exists"))
	}
	color := "ededed"
	if c, ok := in["color"].(string); ok && colorRE.MatchString(c) {
		color = strings.ToLower(strings.TrimPrefix(c, "#"))
	}
	l := &Label{ID: nextID("label"), Name: name, Color: color, Description: optString(in, "description")}
	r.Labels[strings.ToLower(name)] = l
	return l
}

func findLabel(r *Repo, name string) *Label { return r.Labels[strings.ToLower(name)] }

// labelKeys maps label names (strings or {name}) to keys; an unknown label is a 422.
func labelKeys(r *Repo, value any) []string {
	if value == nil {
		return []string{}
	}
	list, ok := value.([]any)
	if !ok {
		if ss, isStrings := value.([]string); isStrings {
			for _, s := range ss {
				list = append(list, s)
			}
		} else {
			panic(validation("Validation Failed", "Issue", "labels", "invalid"))
		}
	}
	keys := []string{}
	for _, item := range list {
		var name string
		switch x := item.(type) {
		case string:
			name = x
		case map[string]any:
			s, isStr := x["name"].(string)
			if !isStr {
				panic(validation("Validation Failed", "Issue", "labels", "invalid"))
			}
			name = s
		default:
			panic(validation("Validation Failed", "Issue", "labels", "invalid"))
		}
		l := findLabel(r, name)
		if l == nil {
			panic(&HTTPError{Status: 422, Message: "Validation Failed", Extra: Obj{{"errors", []any{Obj{{"value", name}, {"resource", "Label"}, {"field", "name"}, {"code", "invalid"}, {"message", "label " + name + " does not exist"}}}}}})
		}
		key := strings.ToLower(l.Name)
		dup := false
		for _, k := range keys {
			if k == key {
				dup = true
			}
		}
		if !dup {
			keys = append(keys, key)
		}
	}
	return keys
}

// ---------------------------------------------------------------- issues

func newIssue(r *Repo, title string, body *string, labels []string, isPull bool) *Issue {
	seq, iso := tick()
	i := &Issue{ID: nextID("issue"), Number: r.NextNumber, Title: title, Body: body, State: "open", Labels: labels, Seq: seq, CreatedAt: iso, UpdatedAt: iso, IsPull: isPull}
	r.NextNumber++
	r.Issues[i.Number] = i
	return i
}

func createIssue(r *Repo, in map[string]any) *Issue {
	var title string
	switch t := in["title"].(type) {
	case string:
		title = t
	case float64:
		title = fmt.Sprint(t)
	}
	if strings.TrimSpace(title) == "" {
		panic(validation("Validation Failed", "Issue", "title", "missing_field"))
	}
	if b, ok := in["body"]; ok && b != nil {
		if _, isStr := b.(string); !isStr {
			panic(validation("Validation Failed", "Issue", "body", "invalid"))
		}
	}
	labels := labelKeys(r, in["labels"])
	return newIssue(r, title, optString(in, "body"), labels, false)
}

func requireIssue(r *Repo, number int) *Issue {
	i := r.Issues[number]
	if i == nil {
		fail(404, "Not Found")
	}
	return i
}

func setIssueState(i *Issue, state string, reason *string) {
	if i.State == state {
		return
	}
	_, iso := tick()
	i.State = state
	i.UpdatedAt = iso
	if state == "closed" {
		i.ClosedAt = strPtr(iso)
		if reason != nil {
			i.StateReason = reason
		} else {
			i.StateReason = strPtr("completed")
		}
	} else {
		i.ClosedAt = nil
		if reason == nil {
			i.StateReason = nil
		} else {
			i.StateReason = strPtr("reopened")
		}
	}
}

func updateIssue(r *Repo, i *Issue, in map[string]any) *Issue {
	if v, ok := in["title"]; ok {
		t, isStr := v.(string)
		if !isStr || strings.TrimSpace(t) == "" {
			panic(validation("Validation Failed", "Issue", "title", "invalid"))
		}
		i.Title = t
	}
	if v, ok := in["body"]; ok {
		if s, isStr := v.(string); isStr {
			i.Body = &s
		} else {
			i.Body = nil
		}
	}
	if v, ok := in["labels"]; ok {
		i.Labels = labelKeys(r, v)
	}
	if v, ok := in["state"]; ok {
		state, _ := v.(string)
		if state != "open" && state != "closed" {
			panic(validation("Validation Failed", "Issue", "state", "invalid"))
		}
		setIssueState(i, state, optString(in, "state_reason"))
	}
	_, i.UpdatedAt = tick()
	return i
}

func addComment(i *Issue, body any) *Comment {
	text, ok := body.(string)
	if !ok || text == "" {
		panic(validation("Validation Failed", "IssueComment", "body", "missing_field"))
	}
	_, iso := tick()
	c := &Comment{ID: nextID("comment"), Body: text, CreatedAt: iso, UpdatedAt: iso}
	i.Comments = append(i.Comments, c)
	i.UpdatedAt = iso
	return c
}

func findComment(r *Repo, id int) (*Issue, *Comment) {
	for _, i := range r.SortedIssues() {
		for _, c := range i.Comments {
			if c.ID == id {
				return i, c
			}
		}
	}
	return nil, nil
}

// ---------------------------------------------------------------- refs and contents

func createRef(r *Repo, ref any, sha any) string {
	name, ok := ref.(string)
	if !ok || !strings.HasPrefix(name, "refs/") || len(strings.Split(name, "/")) < 3 {
		fail(422, "Reference name must start with 'refs/' and have at least two slashes.")
	}
	id, ok := sha.(string)
	if !ok || r.Get(id) == nil {
		fail(422, "Object does not exist")
	}
	if _, exists := r.Refs[name]; exists {
		fail(422, "Reference already exists")
	}
	r.SetRef(name, id)
	return name
}

func updateRef(r *Repo, ref string, sha any, force bool) {
	current, ok := r.Refs[ref]
	if !ok {
		fail(422, "Reference does not exist")
	}
	id, isStr := sha.(string)
	if !isStr || r.Get(id) == nil {
		fail(422, "Object does not exist")
	}
	if !force && !r.IsAncestor(r.Peel(current), r.Peel(id)) {
		fail(422, "Update is not a fast forward")
	}
	r.SetRef(ref, id)
}

func normalizePath(path string) string {
	var parts []string
	for _, p := range strings.Split(path, "/") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	clean := strings.Join(parts, "/")
	if clean == "" {
		panic(validation("path is invalid", "Content", "path", "invalid"))
	}
	for _, p := range parts {
		if p == "." || p == ".." || p == ".git" {
			panic(validation("path is invalid", "Content", "path", "invalid"))
		}
	}
	return clean
}

type FileCommit struct {
	Created  bool
	Path     string
	BlobID   string
	Size     int
	CommitID string
	Branch   string
}

func writeTarget(r *Repo, branch any) (string, string, bool) {
	name := r.DefaultBranch
	if b, ok := branch.(string); ok && b != "" {
		name = strings.TrimPrefix(b, "refs/heads/")
	}
	tip, ok := r.BranchTip(name)
	if !ok && !r.IsEmpty() {
		fail(404, "Branch "+name+" not found")
	}
	return name, tip, ok
}

func putFile(r *Repo, rawPath string, in map[string]any) FileCommit {
	path := normalizePath(rawPath)
	contentText, ok := in["content"].(string)
	if !ok {
		panic(validation("Invalid request.\n\n\"content\" wasn't supplied.", "Content", "content", "missing_field"))
	}
	content, err := base64.StdEncoding.DecodeString(strings.Map(func(c rune) rune {
		if c == '\n' || c == '\r' || c == ' ' {
			return -1
		}
		return c
	}, contentText))
	if err != nil {
		content, _ = base64.RawStdEncoding.DecodeString(strings.TrimRight(contentText, "="))
	}
	message, _ := in["message"].(string)
	if message == "" {
		message = "Update " + path
	}
	branch, tip, hasTip := writeTarget(r, in["branch"])
	files := map[string]FlatEntry{}
	if hasTip {
		files = r.Flatten(r.Commit(tip).Tree)
	}
	existing, exists := files[path]
	if exists {
		sha, _ := in["sha"].(string)
		if sha == "" {
			panic(validation("Invalid request.\n\n\"sha\" wasn't supplied.", "Content", "sha", "missing_field"))
		}
		if sha != existing.ID {
			fail(409, path+" does not match "+sha)
		}
	} else {
		for other := range files {
			if strings.HasPrefix(other, path+"/") || strings.HasPrefix(path, other+"/") {
				panic(validation("Validation Failed", "Content", "path", "invalid"))
			}
		}
	}
	blobID := r.Put("blob", content)
	mode := "100644"
	if exists {
		mode = existing.Mode
	}
	files[path] = FlatEntry{Mode: mode, ID: blobID}
	tree := r.WriteFlat(files)
	var parents []string
	if hasTip {
		parents = []string{tip}
	}
	commitID := r.MakeCommit(tree, parents, message)
	r.SetRef("refs/heads/"+branch, commitID)
	return FileCommit{Created: !exists, Path: path, BlobID: blobID, Size: len(content), CommitID: commitID, Branch: branch}
}

func deleteFile(r *Repo, rawPath string, in map[string]any) (string, string) {
	path := normalizePath(rawPath)
	branch, tip, hasTip := writeTarget(r, in["branch"])
	if !hasTip {
		fail(404, "Not Found")
	}
	files := r.Flatten(r.Commit(tip).Tree)
	existing, exists := files[path]
	if !exists {
		fail(404, "Not Found")
	}
	sha, _ := in["sha"].(string)
	if sha == "" {
		panic(validation("Invalid request.\n\n\"sha\" wasn't supplied.", "Content", "sha", "missing_field"))
	}
	if sha != existing.ID {
		fail(409, path+" does not match "+sha)
	}
	delete(files, path)
	tree := r.WriteFlat(files)
	message, _ := in["message"].(string)
	if message == "" {
		message = "Delete " + path
	}
	commitID := r.MakeCommit(tree, []string{tip}, message)
	r.SetRef("refs/heads/"+branch, commitID)
	return commitID, branch
}

// ---------------------------------------------------------------- pull requests

func branchName(r *Repo, value any, field string) string {
	s, ok := value.(string)
	if !ok || strings.TrimSpace(s) == "" {
		panic(validation("Validation Failed", "PullRequest", field, "missing_field"))
	}
	name := strings.TrimSpace(s)
	if colon := strings.IndexByte(name, ':'); colon >= 0 {
		if !strings.EqualFold(name[:colon], r.Owner) {
			panic(validation("Validation Failed", "PullRequest", field, "invalid"))
		}
		name = name[colon+1:]
	}
	return strings.TrimPrefix(name, "refs/heads/")
}

func createPull(r *Repo, in map[string]any) *Pull {
	title, _ := in["title"].(string)
	if strings.TrimSpace(title) == "" {
		panic(validation("Validation Failed", "PullRequest", "title", "missing_field"))
	}
	head := branchName(r, in["head"], "head")
	base := branchName(r, in["base"], "base")
	headTip, headOK := r.BranchTip(head)
	baseTip, baseOK := r.BranchTip(base)
	if !baseOK {
		panic(validation("Validation Failed", "PullRequest", "base", "invalid"))
	}
	if !headOK {
		panic(validation("Validation Failed", "PullRequest", "head", "invalid"))
	}
	if head == base || r.IsAncestor(headTip, baseTip) {
		panic(customError("No commits between " + base + " and " + head))
	}
	for _, p := range r.SortedPulls() {
		if p.Issue.State == "open" && p.HeadRef == head && p.BaseRef == base {
			panic(customError("A pull request already exists for " + r.Owner + ":" + head + "."))
		}
	}
	issue := newIssue(r, title, optString(in, "body"), []string{}, true)
	p := &Pull{ID: nextID("pull"), Number: issue.Number, Issue: issue, HeadRef: head, BaseRef: base, Draft: in["draft"] == true}
	r.Pulls[p.Number] = p
	return p
}

func requirePull(r *Repo, number int) *Pull {
	p := r.Pulls[number]
	if p == nil {
		fail(404, "Not Found")
	}
	return p
}

func pullHeadSha(r *Repo, p *Pull) string {
	if p.ClosedHeadSha != nil {
		return *p.ClosedHeadSha
	}
	if tip, ok := r.BranchTip(p.HeadRef); ok {
		return tip
	}
	return zeroID
}

func pullBaseSha(r *Repo, p *Pull) string {
	if p.ClosedBaseSha != nil {
		return *p.ClosedBaseSha
	}
	if tip, ok := r.BranchTip(p.BaseRef); ok {
		return tip
	}
	return zeroID
}

func updatePull(r *Repo, p *Pull, in map[string]any) *Pull {
	if v, ok := in["title"]; ok {
		t, isStr := v.(string)
		if !isStr || strings.TrimSpace(t) == "" {
			panic(validation("Validation Failed", "PullRequest", "title", "invalid"))
		}
		p.Issue.Title = t
	}
	if v, ok := in["body"]; ok {
		if s, isStr := v.(string); isStr {
			p.Issue.Body = &s
		} else {
			p.Issue.Body = nil
		}
	}
	if b, ok := in["base"].(string); ok {
		base := branchName(r, b, "base")
		if _, exists := r.BranchTip(base); !exists {
			panic(validation("Validation Failed", "PullRequest", "base", "invalid"))
		}
		p.BaseRef = base
	}
	if state, ok := in["state"].(string); ok && (state == "open" || state == "closed") {
		if p.Merged {
			panic(validation("Validation Failed", "PullRequest", "state", "invalid"))
		}
		if state == "closed" {
			h, b := pullHeadSha(r, p), pullBaseSha(r, p)
			p.ClosedHeadSha, p.ClosedBaseSha = &h, &b
		} else {
			p.ClosedHeadSha, p.ClosedBaseSha = nil, nil
		}
		setIssueState(p.Issue, state, nil)
	}
	_, p.Issue.UpdatedAt = tick()
	return p
}

// mergeTrees is a three-way merge of flat trees, file by file; "" when a path changed on both sides.
func mergeTrees(r *Repo, baseTree string, hasBase bool, ours, theirs string) string {
	if hasBase && baseTree == theirs {
		return ours
	}
	if hasBase && baseTree == ours {
		return theirs
	}
	base := map[string]FlatEntry{}
	if hasBase {
		base = r.Flatten(baseTree)
	}
	o := r.Flatten(ours)
	t := r.Flatten(theirs)
	paths := map[string]bool{}
	for _, m := range []map[string]FlatEntry{base, o, t} {
		for p := range m {
			paths[p] = true
		}
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	same := func(a FlatEntry, aok bool, b FlatEntry, bok bool) bool {
		if !aok {
			return !bok
		}
		return bok && a.ID == b.ID && a.Mode == b.Mode
	}
	result := map[string]FlatEntry{}
	for _, p := range sorted {
		b, bok := base[p]
		oe, ook := o[p]
		te, tok := t[p]
		var pick FlatEntry
		var pok bool
		switch {
		case same(oe, ook, te, tok):
			pick, pok = oe, ook
		case same(b, bok, oe, ook):
			pick, pok = te, tok
		case same(b, bok, te, tok):
			pick, pok = oe, ook
		default:
			return ""
		}
		if pok {
			result[p] = pick
		}
	}
	for p := range result {
		for cut := strings.LastIndexByte(p, '/'); cut > 0; cut = strings.LastIndexByte(p[:cut], '/') {
			if _, clash := result[p[:cut]]; clash {
				return ""
			}
		}
	}
	return r.WriteFlat(result)
}

var mergeCache = map[string]string{}

// mergedTree is the merged tree of head into base, or "" on a conflict.
func mergedTree(r *Repo, baseTip, headTip string) string {
	key := fmt.Sprintf("%d:%s:%s", r.ID, baseTip, headTip)
	if tree, ok := mergeCache[key]; ok {
		return tree
	}
	ancestor, hasAncestor := r.MergeBase(baseTip, headTip)
	baseTree := ""
	if hasAncestor {
		baseTree = r.Commit(ancestor).Tree
	}
	tree := mergeTrees(r, baseTree, hasAncestor, r.Commit(baseTip).Tree, r.Commit(headTip).Tree)
	mergeCache[key] = tree
	return tree
}

// pullMergeable: 1 mergeable, 0 conflicting, -1 unknown (merged or closed).
func pullMergeable(r *Repo, p *Pull) int {
	if p.Merged || p.Issue.State != "open" {
		return -1
	}
	head, hok := r.BranchTip(p.HeadRef)
	base, bok := r.BranchTip(p.BaseRef)
	if !hok || !bok {
		return 0
	}
	if mergedTree(r, base, head) == "" {
		return 0
	}
	return 1
}

func mergePull(r *Repo, p *Pull, method, title, message, sha string) (string, string) {
	if p.Merged || p.Issue.State != "open" {
		fail(405, "Pull Request is not mergeable")
	}
	headTip, hok := r.BranchTip(p.HeadRef)
	baseTip, bok := r.BranchTip(p.BaseRef)
	if !hok || !bok {
		fail(405, "Pull Request is not mergeable")
	}
	if sha != "" && sha != headTip {
		fail(409, "Head branch was modified. Review and try the merge again.")
	}
	if method == "" {
		method = "merge"
	}
	method = strings.ToLower(method)
	if method != "merge" && method != "squash" && method != "rebase" {
		panic(validation("Validation Failed", "PullRequest", "merge_method", "invalid"))
	}
	tree := mergedTree(r, baseTip, headTip)
	if tree == "" {
		fail(405, "Pull Request is not mergeable")
	}
	extra := ""
	if message != "" {
		extra = "\n\n" + message
	}
	var commitID string
	if method == "merge" {
		if title == "" {
			title = fmt.Sprintf("Merge pull request #%d from %s/%s", p.Number, r.Owner, p.HeadRef)
		}
		commitID = r.MakeCommit(tree, []string{baseTip, headTip}, title+"\n\n"+p.Issue.Title+extra)
	} else {
		if title == "" {
			title = fmt.Sprintf("%s (#%d)", p.Issue.Title, p.Number)
		}
		commitID = r.MakeCommit(tree, []string{baseTip}, title+extra)
	}
	r.SetRef("refs/heads/"+p.BaseRef, commitID)
	_, iso := tick()
	p.Merged = true
	p.MergedAt = strPtr(iso)
	p.MergeCommitSha = strPtr(commitID)
	p.ClosedHeadSha = strPtr(headTip)
	p.ClosedBaseSha = strPtr(baseTip)
	setIssueState(p.Issue, "closed", nil)
	p.Issue.StateReason = nil
	return commitID, "Pull Request successfully merged"
}

// ---------------------------------------------------------------- pull request files

type ChangedFile struct {
	Filename                     string
	Status                       string
	Additions, Deletions, Changes int
	Sha                          string
}

func splitLines(data []byte) ([]string, bool) {
	for _, b := range data {
		if b == 0 {
			return nil, false
		}
	}
	text := string(data)
	if text == "" {
		return []string{}, true
	}
	parts := strings.Split(text, "\n")
	if parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts, true
}

// editDistance is Myers' shortest edit distance between two line lists.
func editDistance(a, b []string) int {
	n, m := len(a), len(b)
	max := n + m
	if max == 0 {
		return 0
	}
	limit := max
	if limit > 4000 {
		limit = 4000
	}
	offset := limit + 1
	v := make([]int, 2*limit+3)
	for d := 0; d <= limit; d++ {
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
				x = v[offset+k+1]
			} else {
				x = v[offset+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[offset+k] = x
			if x >= n && y >= m {
				return d
			}
		}
	}
	counts := map[string]int{}
	for _, l := range a {
		counts[l]++
	}
	common := 0
	for _, l := range b {
		if counts[l] > 0 {
			common++
			counts[l]--
		}
	}
	return n + m - 2*common
}

func lineCounts(r *Repo, oldID, newID string) (int, int) {
	var oldLines, newLines []string
	ok1, ok2 := true, true
	if oldID != "" {
		if obj := r.Get(oldID); obj != nil {
			oldLines, ok1 = splitLines(obj.Data)
		}
	}
	if newID != "" {
		if obj := r.Get(newID); obj != nil {
			newLines, ok2 = splitLines(obj.Data)
		}
	}
	if !ok1 || !ok2 {
		return 0, 0
	}
	d := editDistance(oldLines, newLines)
	common := (len(oldLines) + len(newLines) - d) / 2
	return len(newLines) - common, len(oldLines) - common
}

func pullFiles(r *Repo, p *Pull) []ChangedFile {
	head := pullHeadSha(r, p)
	base := pullBaseSha(r, p)
	if r.Commit(head) == nil || r.Commit(base) == nil {
		return []ChangedFile{}
	}
	ancestor := base
	if p.Merged {
		if p.ClosedBaseSha != nil {
			ancestor = *p.ClosedBaseSha
		}
	} else if mb, ok := r.MergeBase(base, head); ok {
		ancestor = mb
	}
	from := map[string]FlatEntry{}
	if c := r.Commit(ancestor); c != nil {
		from = r.Flatten(c.Tree)
	}
	to := r.Flatten(r.Commit(head).Tree)
	set := map[string]bool{}
	for k := range from {
		set[k] = true
	}
	for k := range to {
		set[k] = true
	}
	paths := make([]string, 0, len(set))
	for k := range set {
		paths = append(paths, k)
	}
	sort.Strings(paths)
	files := []ChangedFile{}
	for _, path := range paths {
		a, aok := from[path]
		b, bok := to[path]
		if aok && bok && a.ID == b.ID && a.Mode == b.Mode {
			continue
		}
		oldID, newID := "", ""
		if aok {
			oldID = a.ID
		}
		if bok {
			newID = b.ID
		}
		add, del := lineCounts(r, oldID, newID)
		status := "modified"
		if !aok {
			status = "added"
		} else if !bok {
			status = "removed"
		}
		sha := newID
		if sha == "" {
			sha = oldID
		}
		files = append(files, ChangedFile{Filename: path, Status: status, Additions: add, Deletions: del, Changes: add + del, Sha: sha})
	}
	return files
}

// pullCommits is the head commits the base does not have, oldest first.
func pullCommits(r *Repo, p *Pull) []string {
	head := pullHeadSha(r, p)
	base := pullBaseSha(r, p)
	if p.Merged && p.ClosedBaseSha != nil {
		base = *p.ClosedBaseSha
	}
	if r.Commit(head) == nil {
		return []string{}
	}
	excluded := map[string]bool{}
	if r.Commit(base) != nil {
		for _, id := range r.Log(base, 0, nil) {
			excluded[id] = true
		}
	}
	log := r.Log(head, 250, excluded)
	for i, j := 0, len(log)-1; i < j; i, j = i+1, j-1 {
		log[i], log[j] = log[j], log[i]
	}
	return log
}
