package main

// The forge's whole state, in memory: repositories with their git objects and refs, and the issue
// tracker beside them. Ids and timestamps come from counters, never the clock. Every listing sorts
// explicitly: Go's map order is random and must never reach a reply.

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type Label struct {
	ID          int
	Name        string
	Color       string
	Description *string
}

type Comment struct {
	ID        int
	Body      string
	CreatedAt string
	UpdatedAt string
}

type Issue struct {
	ID          int
	Number      int
	Title       string
	Body        *string
	State       string // open, closed
	StateReason *string
	Labels      []string // label keys (lowercase names)
	Comments    []*Comment
	Seq         int64
	CreatedAt   string
	UpdatedAt   string
	ClosedAt    *string
	IsPull      bool
}

type Pull struct {
	ID             int
	Number         int
	Issue          *Issue
	HeadRef        string
	BaseRef        string
	Merged         bool
	MergedAt       *string
	MergeCommitSha *string
	ClosedHeadSha  *string
	ClosedBaseSha  *string
	Draft          bool
}

var seqCounter int64

// tick is the next sequence number and its timestamp.
func tick() (int64, string) {
	seqCounter++
	return seqCounter, isoAt(seqCounter)
}

var idCounters = map[string]int{}

func nextID(kind string) int {
	idCounters[kind]++
	return idCounters[kind]
}

func strPtr(s string) *string { return &s }

// ---------------------------------------------------------------- repositories

type FlatEntry struct {
	Mode string
	ID   string
}

type Repo struct {
	ID            int
	Owner         string
	Name          string
	Description   *string
	Homepage      *string
	IsPrivate     bool
	DefaultBranch string
	CreatedAt     string
	UpdatedAt     string
	PushedAt      *string
	Seq           int64

	Objects     map[string]*GitObject
	Refs        map[string]string
	RefsVersion int

	Labels     map[string]*Label
	Issues     map[int]*Issue
	Pulls      map[int]*Pull
	NextNumber int

	commitCache map[string]*Commit
}

func newRepo(owner, name string, description *string, isPrivate bool) *Repo {
	seq, iso := tick()
	return &Repo{
		ID: nextID("repo"), Owner: owner, Name: name, Description: description, IsPrivate: isPrivate,
		DefaultBranch: config.DefaultBranch, CreatedAt: iso, UpdatedAt: iso, Seq: seq,
		Objects: map[string]*GitObject{}, Refs: map[string]string{}, Labels: map[string]*Label{},
		Issues: map[int]*Issue{}, Pulls: map[int]*Pull{}, NextNumber: 1, commitCache: map[string]*Commit{},
	}
}

func (r *Repo) FullName() string    { return r.Owner + "/" + r.Name }
func (r *Repo) HeadRefName() string { return "refs/heads/" + r.DefaultBranch }

func (r *Repo) IsEmpty() bool {
	for name := range r.Refs {
		if strings.HasPrefix(name, "refs/heads/") {
			return false
		}
	}
	return true
}

func (r *Repo) Get(id string) *GitObject { return r.Objects[id] }

func (r *Repo) Put(typ string, data []byte) string {
	id := hashObject(typ, data)
	if _, ok := r.Objects[id]; !ok {
		r.Objects[id] = &GitObject{Type: typ, Data: data}
	}
	return id
}

func (r *Repo) Commit(id string) *Commit {
	if c, ok := r.commitCache[id]; ok {
		return c
	}
	obj := r.Objects[id]
	if obj == nil || obj.Type != "commit" {
		return nil
	}
	c := parseCommit(obj.Data)
	r.commitCache[id] = &c
	return &c
}

func (r *Repo) Tree(id string) []TreeEntry {
	obj := r.Objects[id]
	if obj == nil || obj.Type != "tree" {
		return nil
	}
	entries, _ := parseTree(obj.Data)
	return entries
}

// Peel follows tag objects down to the first non-tag object id.
func (r *Repo) Peel(id string) string {
	current := id
	for i := 0; i < 100; i++ {
		obj := r.Objects[current]
		if obj == nil || obj.Type != "tag" {
			return current
		}
		current = parseTag(obj.Data).Object
	}
	return current
}

func (r *Repo) SetRef(name, id string) {
	if id == "" {
		delete(r.Refs, name)
	} else {
		r.Refs[name] = id
	}
	r.RefsVersion++
	_, iso := tick()
	r.PushedAt = strPtr(iso)
	r.UpdatedAt = iso
}

func (r *Repo) BranchTip(branch string) (string, bool) {
	id, ok := r.Refs["refs/heads/"+branch]
	return id, ok
}

// SortedRefs is every ref, by name.
func (r *Repo) SortedRefs() [][2]string {
	out := make([][2]string, 0, len(r.Refs))
	for name, id := range r.Refs {
		out = append(out, [2]string{name, id})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

var shortHexRE = regexp.MustCompile(`^[0-9a-f]{4,39}$`)

// Resolve maps a branch, tag, full ref or commit id to a commit id ("" = the default branch tip).
func (r *Repo) Resolve(rev string) (string, bool) {
	if rev == "" {
		return r.BranchTip(r.DefaultBranch)
	}
	for _, name := range []string{rev, "refs/heads/" + rev, "refs/tags/" + rev} {
		if id, ok := r.Refs[name]; ok {
			return r.Peel(id), true
		}
	}
	if isObjectID(rev) {
		if _, ok := r.Objects[rev]; ok {
			return r.Peel(rev), true
		}
	}
	if shortHexRE.MatchString(rev) {
		var found []string
		for id := range r.Objects {
			if strings.HasPrefix(id, rev) {
				found = append(found, id)
			}
		}
		if len(found) > 0 {
			sort.Strings(found)
			return r.Peel(found[0]), true
		}
	}
	return "", false
}

// Flatten maps every file path under a tree to its entry.
func (r *Repo) Flatten(treeID string) map[string]FlatEntry {
	out := map[string]FlatEntry{}
	r.flattenInto(treeID, "", out)
	return out
}

func (r *Repo) flattenInto(treeID, prefix string, out map[string]FlatEntry) {
	for _, e := range r.Tree(treeID) {
		path := e.Name
		if prefix != "" {
			path = prefix + "/" + e.Name
		}
		if isTreeMode(e.Mode) {
			r.flattenInto(e.ID, path, out)
		} else {
			out[path] = FlatEntry{Mode: e.Mode, ID: e.ID}
		}
	}
}

// WriteFlat builds (and stores) the nested trees of a flat path map; it returns the root tree id.
func (r *Repo) WriteFlat(files map[string]FlatEntry) string {
	type dir struct {
		files []TreeEntry
		dirs  map[string]*dir
		order []string
	}
	newDir := func() *dir { return &dir{dirs: map[string]*dir{}} }
	root := newDir()
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, path := range paths {
		parts := strings.Split(path, "/")
		d := root
		for _, part := range parts[:len(parts)-1] {
			next, ok := d.dirs[part]
			if !ok {
				next = newDir()
				d.dirs[part] = next
				d.order = append(d.order, part)
			}
			d = next
		}
		e := files[path]
		d.files = append(d.files, TreeEntry{Mode: e.Mode, Name: parts[len(parts)-1], ID: e.ID})
	}
	var write func(d *dir) string
	write = func(d *dir) string {
		entries := append([]TreeEntry(nil), d.files...)
		for _, name := range d.order {
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, ID: write(d.dirs[name])})
		}
		return r.Put("tree", buildTree(entries))
	}
	return write(root)
}

// EntryAt is the tree entry at path under a root tree ("" is the root itself).
func (r *Repo) EntryAt(rootTree, path string) (FlatEntry, bool) {
	current := FlatEntry{Mode: "40000", ID: rootTree}
	for _, part := range strings.Split(path, "/") {
		if part == "" {
			continue
		}
		if !isTreeMode(current.Mode) {
			return FlatEntry{}, false
		}
		found := false
		for _, e := range r.Tree(current.ID) {
			if e.Name == part {
				current = FlatEntry{Mode: e.Mode, ID: e.ID}
				found = true
				break
			}
		}
		if !found {
			return FlatEntry{}, false
		}
	}
	return current, true
}

// MakeCommit commits as the viewer at the next tick.
func (r *Repo) MakeCommit(tree string, parents []string, message string) string {
	seq, _ := tick()
	sig := makeSignature(config.Viewer.Name, config.Viewer.Email, config.NowSec+seq)
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	return r.Put("commit", buildCommit(Commit{Tree: tree, Parents: parents, Author: sig, Committer: sig, Message: message}))
}

// IsAncestor reports whether ancestor is reachable from descendant (a commit is its own ancestor).
func (r *Repo) IsAncestor(ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	seen := map[string]bool{descendant: true}
	queue := []string{descendant}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		c := r.Commit(id)
		if c == nil {
			continue
		}
		for _, p := range c.Parents {
			if p == ancestor {
				return true
			}
			if !seen[p] {
				seen[p] = true
				queue = append(queue, p)
			}
		}
	}
	return false
}

// MergeBase is a best common ancestor (nearest to b in breadth-first order).
func (r *Repo) MergeBase(a, b string) (string, bool) {
	ancestors := map[string]bool{}
	queue := []string{a}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if ancestors[id] {
			continue
		}
		ancestors[id] = true
		if c := r.Commit(id); c != nil {
			queue = append(queue, c.Parents...)
		}
	}
	seen := map[string]bool{}
	walk := []string{b}
	for len(walk) > 0 {
		id := walk[0]
		walk = walk[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		if ancestors[id] {
			return id, true
		}
		if c := r.Commit(id); c != nil {
			walk = append(walk, c.Parents...)
		}
	}
	return "", false
}

var committerTimeRE = regexp.MustCompile(` (\d+) [+-]\d{4}$`)

// Log is the commits reachable from tip, newest first by committer time (ties: discovery order).
func (r *Repo) Log(tip string, limit int, stopAt map[string]bool) []string {
	type pending struct {
		id    string
		time  int64
		order int
	}
	var out []string
	seen := map[string]bool{}
	var queue []pending
	order := 0
	push := func(id string) {
		if seen[id] || stopAt[id] {
			return
		}
		seen[id] = true
		c := r.Commit(id)
		if c == nil {
			return
		}
		var t int64
		if m := committerTimeRE.FindStringSubmatch(c.Committer); m != nil {
			t, _ = strconv.ParseInt(m[1], 10, 64)
		}
		queue = append(queue, pending{id, t, order})
		order++
	}
	push(tip)
	for len(queue) > 0 && (limit <= 0 || len(out) < limit) {
		best := 0
		for i := 1; i < len(queue); i++ {
			p, q := queue[i], queue[best]
			if p.time > q.time || (p.time == q.time && p.order < q.order) {
				best = i
			}
		}
		next := queue[best]
		queue = append(queue[:best], queue[best+1:]...)
		out = append(out, next.id)
		for _, p := range r.Commit(next.id).Parents {
			push(p)
		}
	}
	return out
}

// SortedIssues is every issue (and pull request issue), by number.
func (r *Repo) SortedIssues() []*Issue {
	out := make([]*Issue, 0, len(r.Issues))
	for _, i := range r.Issues {
		out = append(out, i)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Number < out[b].Number })
	return out
}

func (r *Repo) SortedPulls() []*Pull {
	out := make([]*Pull, 0, len(r.Pulls))
	for _, p := range r.Pulls {
		out = append(out, p)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Number < out[b].Number })
	return out
}

func (r *Repo) SortedLabels() []*Label {
	out := make([]*Label, 0, len(r.Labels))
	for _, l := range r.Labels {
		out = append(out, l)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out
}

// ---------------------------------------------------------------- the store

type Store struct {
	repos map[string]*Repo
}

var store = &Store{repos: map[string]*Repo{}}

func repoKey(owner, name string) string { return strings.ToLower(owner) + "/" + strings.ToLower(name) }

func (s *Store) Find(owner, name string) *Repo {
	name = strings.TrimSuffix(name, ".git")
	return s.repos[repoKey(owner, name)]
}

func (s *Store) Create(owner, name string, description *string, isPrivate bool) *Repo {
	r := newRepo(owner, name, description, isPrivate)
	s.repos[repoKey(owner, name)] = r
	return r
}

func (s *Store) Delete(r *Repo) { delete(s.repos, repoKey(r.Owner, r.Name)) }

func (s *Store) Rename(r *Repo, name string) {
	delete(s.repos, repoKey(r.Owner, r.Name))
	r.Name = name
	s.repos[repoKey(r.Owner, name)] = r
}

// List is the owner's repositories, oldest first.
func (s *Store) List(owner string) []*Repo {
	lower := strings.ToLower(owner)
	var out []*Repo
	for _, r := range s.repos {
		if strings.ToLower(r.Owner) == lower {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Seq < out[b].Seq })
	return out
}

// All is every repository, oldest first.
func (s *Store) All() []*Repo {
	out := make([]*Repo, 0, len(s.repos))
	for _, r := range s.repos {
		out = append(out, r)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Seq < out[b].Seq })
	return out
}

func (s *Store) ByID(id int) *Repo {
	for _, r := range s.repos {
		if r.ID == id {
			return r
		}
	}
	return nil
}

func isViewer(owner string) bool { return strings.EqualFold(owner, config.Viewer.Login) }

// lookupRepo is owner/name if the viewer may see it (another owner's name is simply not found).
func lookupRepo(owner, name string) *Repo {
	if !isViewer(owner) {
		return nil
	}
	return store.Find(owner, name)
}
