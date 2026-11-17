package main

// REST v3 under /api/v3 (the Go twin of src/rest/router.ts). Errors are JSON {message}; every reply
// carries X-GitHub-Request-Id.

import (
	"encoding/base64"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type restCtx struct {
	r    *Request
	u    Urls
	body map[string]any
}

func notFound() *HTTPError { return httpErr(404, "Not Found") }

func (c *restCtx) q(name string) (string, bool) {
	v, ok := c.r.Query()[name]
	if !ok || len(v) == 0 {
		return "", false
	}
	return v[0], true
}

func (c *restCtx) qDefault(name, def string) string {
	if v, ok := c.q(name); ok {
		return v
	}
	return def
}

// ---------------------------------------------------------------- pagination

func pageParams(c *restCtx) (int, int) {
	per, err := strconv.Atoi(c.qDefault("per_page", "30"))
	if err != nil || per < 1 {
		per = 30
	}
	if per > 100 {
		per = 100
	}
	page, err := strconv.Atoi(c.qDefault("page", "1"))
	if err != nil || page < 1 {
		page = 1
	}
	return per, page
}

// sendPage slices a list to the requested page and answers it with GitHub's Link header.
func sendPage(c *restCtx, n int, view func(i int) any, path string) *Response {
	per, page := pageParams(c)
	last := (n + per - 1) / per
	if last < 1 {
		last = 1
	}
	items := []any{}
	for i := (page - 1) * per; i < n && i < page*per; i++ {
		items = append(items, view(i))
	}
	var links []string
	link := func(p int, rel string) {
		params := url.Values{}
		for k, v := range c.r.Query() {
			params[k] = append([]string(nil), v...)
		}
		params.Set("per_page", strconv.Itoa(per))
		params.Set("page", strconv.Itoa(p))
		links = append(links, fmt.Sprintf(`<%s%s?%s>; rel="%s"`, c.u.API, path, params.Encode(), rel))
	}
	if page < last {
		link(page+1, "next")
		link(last, "last")
	}
	if page > 1 {
		link(1, "first")
		prev := page - 1
		if prev > last {
			prev = last
		}
		link(prev, "prev")
	}
	resp := jsonResp(200, items)
	if len(links) > 0 {
		resp.Add("link", strings.Join(links, ", "))
	}
	return resp
}

func intParam(s string) int {
	if !digitsRE.MatchString(s) || len(s) > 15 {
		panic(notFound())
	}
	n, _ := strconv.Atoi(s)
	return n
}

// ---------------------------------------------------------------- the single-issue read cache

var issuePathRE = regexp.MustCompile(`^/api/v3/repos/([^/]+)/([^/]+)/issues/(\d+)$`)

type cachedBody struct {
	version uint64
	body    []byte
}

var issueCache = map[string]cachedBody{}

// fastIssueGet serves GET /issues/{n} from serialised bytes while no request has mutated the store
// since they were made; nil hands the request to the normal router.
func fastIssueGet(r *Request) *Response {
	if r.Method != "GET" {
		return nil
	}
	m := issuePathRE.FindStringSubmatch(r.Path)
	if m == nil || !isAuthorized(r) {
		return nil
	}
	key := r.Headers.Get("host") + "|" + r.Path
	if c, ok := issueCache[key]; ok && c.version == mutationVersion {
		return jsonBytesResp(200, c.body)
	}
	owner, e1 := url.PathUnescape(m[1])
	name, e2 := url.PathUnescape(m[2])
	if e1 != nil || e2 != nil {
		return nil
	}
	repo := lookupRepo(owner, name)
	if repo == nil {
		return nil
	}
	n, _ := strconv.Atoi(m[3])
	issue := repo.Issues[n]
	if issue == nil {
		return nil
	}
	body := toJSON(issueView(repo, issue, urlsOf(r)))
	if len(issueCache) > 50000 {
		issueCache = map[string]cachedBody{}
	}
	issueCache[key] = cachedBody{mutationVersion, body}
	return jsonBytesResp(200, body)
}

// ---------------------------------------------------------------- entry point

func handleRest(r *Request) (resp *Response) {
	u := urlsOf(r)
	rest := strings.TrimPrefix(r.Path, "/api/v3")
	var segs []string
	for _, s := range strings.Split(rest, "/") {
		if s == "" {
			continue
		}
		d, err := url.PathUnescape(s)
		if err != nil {
			d = s
		}
		segs = append(segs, d)
	}
	if len(segs) == 0 {
		resp = jsonResp(200, Obj{{"current_user_url", u.API + "/user"}, {"repository_url", u.API + "/repos/{owner}/{repo}"}, {"meta_url", u.API + "/meta"}})
		return resp.Add("x-oauth-scopes", "repo, read:org, workflow")
	}
	if !isAuthorized(r) {
		return errorResp(401, "Requires authentication", nil)
	}
	c := &restCtx{r: r, u: u, body: map[string]any{}}
	if r.Method == "POST" || r.Method == "PATCH" || r.Method == "PUT" || r.Method == "DELETE" {
		body, err := parseJSONBody(r.Body)
		if err != nil {
			if r.Method != "DELETE" {
				panic(err)
			}
		} else {
			c.body = body
		}
	}
	return dispatchRest(c, segs)
}

func parseJSONBody(raw []byte) (map[string]any, *HTTPError) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return map[string]any{}, nil
	}
	v, err := parseJSON(raw)
	if err != nil {
		return nil, httpErr(400, "Problems parsing JSON")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, httpErr(400, "Body should be a JSON object")
	}
	return m, nil
}

func dispatchRest(c *restCtx, s []string) *Response {
	m := c.r.Method
	u := c.u
	head := s[0]
	switch {
	case head == "meta" && len(s) == 1:
		return jsonResp(200, Obj{{"verifiable_password_authentication", false}, {"installed_version", config.Version}, {"hooks", []any{}}, {"git", []any{}}, {"web", []any{}}, {"api", []any{}}})
	case head == "rate_limit" && len(s) == 1:
		core := Obj{{"limit", 5000}, {"used", 0}, {"remaining", 5000}, {"reset", config.NowSec + 3600}}
		return jsonResp(200, Obj{{"resources", Obj{{"core", core}, {"graphql", core}, {"search", core}}}, {"rate", core}})
	case head == "zen" && len(s) == 1:
		return jsonResp(200, Obj{{"message", "Keep it logically awesome."}})
	case head == "user" && len(s) == 1 && m == "GET":
		return jsonResp(200, fullUserView(u)).Add("x-oauth-scopes", "repo, read:org, workflow")
	case head == "user" && len(s) == 2 && s[1] == "repos":
		if m == "GET" {
			return listRepos(c, "/user/repos")
		}
		if m == "POST" {
			repo := createRepository(c.body)
			return jsonResp(201, repoView(repo, u)).Add("location", u.API+"/repos/"+repo.FullName())
		}
	case head == "users" && len(s) >= 2:
		if !isViewer(s[1]) {
			panic(notFound())
		}
		if len(s) == 2 && m == "GET" {
			return jsonResp(200, fullUserView(u))
		}
		if len(s) == 3 && s[2] == "repos" && m == "GET" {
			return listRepos(c, "/users/"+s[1]+"/repos")
		}
	case head == "repos" && len(s) >= 3:
		return repoRoutes(c, requireRepo(s[1], s[2]), s[3:])
	}
	panic(notFound())
}

func listRepos(c *restCtx, path string) *Response {
	repos := store.List(config.Viewer.Login)
	switch c.qDefault("visibility", "") {
	case "public":
		repos = filterRepos(repos, func(r *Repo) bool { return !r.IsPrivate })
	case "private":
		repos = filterRepos(repos, func(r *Repo) bool { return r.IsPrivate })
	}
	sortBy := c.qDefault("sort", "full_name")
	defaultDir := "desc"
	if sortBy == "full_name" {
		defaultDir = "asc"
	}
	direction := c.qDefault("direction", defaultDir)
	sorted := append([]*Repo(nil), repos...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sortBy == "full_name" {
			return strings.ToLower(sorted[i].Name) < strings.ToLower(sorted[j].Name)
		}
		return sorted[i].Seq < sorted[j].Seq
	})
	if direction == "desc" {
		for i, j := 0, len(sorted)-1; i < j; i, j = i+1, j-1 {
			sorted[i], sorted[j] = sorted[j], sorted[i]
		}
	}
	return sendPage(c, len(sorted), func(i int) any { return repoView(sorted[i], c.u) }, path)
}

func filterRepos(in []*Repo, keep func(*Repo) bool) []*Repo {
	var out []*Repo
	for _, r := range in {
		if keep(r) {
			out = append(out, r)
		}
	}
	return out
}

// ---------------------------------------------------------------- /repos/{owner}/{repo}/...

func repoRoutes(c *restCtx, repo *Repo, s []string) *Response {
	m := c.r.Method
	u := c.u
	base := "/repos/" + repo.FullName()
	if len(s) == 0 {
		switch m {
		case "GET":
			return jsonResp(200, repoView(repo, u))
		case "PATCH":
			return jsonResp(200, repoView(updateRepository(repo, c.body), u))
		case "DELETE":
			store.Delete(repo)
			return emptyResp(204)
		}
		panic(notFound())
	}
	switch s[0] {
	case "branches":
		return branchRoutes(c, repo, s[1:], base)
	case "commits":
		return commitRoutes(c, repo, s[1:], base)
	case "git":
		return gitDataRoutes(c, repo, s[1:])
	case "contents":
		return contentRoutes(c, repo, strings.Join(s[1:], "/"))
	case "readme":
		tip, ok := repo.Resolve(c.qDefault("ref", ""))
		if ok {
			for _, e := range repo.Tree(repo.Commit(tip).Tree) {
				if !isTreeMode(e.Mode) && regexp.MustCompile(`(?i)^readme(\.|$)`).MatchString(e.Name) {
					return contentRoutes(c, repo, e.Name)
				}
			}
		}
		panic(notFound())
	case "labels":
		return labelRoutes(c, repo, s[1:], base)
	case "issues":
		return issueRoutes(c, repo, s[1:], base)
	case "pulls":
		return pullRoutes(c, repo, s[1:], base)
	case "collaborators":
		if m == "GET" && len(s) == 1 {
			return jsonResp(200, []any{userView(u)})
		}
	case "tags":
		if m == "GET" && len(s) == 1 {
			var tags [][2]string
			for _, ref := range repo.SortedRefs() {
				if strings.HasPrefix(ref[0], "refs/tags/") {
					tags = append(tags, ref)
				}
			}
			return sendPage(c, len(tags), func(i int) any {
				sha := repo.Peel(tags[i][1])
				return Obj{{"name", tags[i][0][10:]}, {"commit", Obj{{"sha", sha}, {"url", u.API + base + "/commits/" + sha}}}}
			}, base+"/tags")
		}
	}
	panic(notFound())
}

func branchRoutes(c *restCtx, repo *Repo, s []string, base string) *Response {
	if c.r.Method != "GET" {
		panic(notFound())
	}
	if len(s) == 0 {
		var branches [][2]string
		for _, ref := range repo.SortedRefs() {
			if strings.HasPrefix(ref[0], "refs/heads/") {
				branches = append(branches, [2]string{ref[0][11:], ref[1]})
			}
		}
		return sendPage(c, len(branches), func(i int) any { return branchView(repo, branches[i][0], branches[i][1], c.u, false) }, base+"/branches")
	}
	name := strings.Join(s, "/")
	tip, ok := repo.BranchTip(name)
	if !ok {
		panic(httpErr(404, "Branch not found"))
	}
	return jsonResp(200, branchView(repo, name, tip, c.u, true))
}

func commitRoutes(c *restCtx, repo *Repo, s []string, base string) *Response {
	if c.r.Method != "GET" {
		panic(notFound())
	}
	if len(s) == 0 {
		if repo.IsEmpty() {
			panic(httpErr(409, "Git Repository is empty."))
		}
		from := c.qDefault("sha", "")
		tip, ok := repo.Resolve(from)
		if !ok {
			panic(httpErr(404, "No commit found for SHA: "+from))
		}
		commits := repo.Log(tip, 10000, nil)
		if path := c.qDefault("path", ""); path != "" {
			var kept []string
			for _, id := range commits {
				cm := repo.Commit(id)
				here, hereOK := repo.EntryAt(cm.Tree, path)
				if len(cm.Parents) == 0 {
					if hereOK {
						kept = append(kept, id)
					}
					continue
				}
				changed := true
				for _, p := range cm.Parents {
					there, thereOK := repo.EntryAt(repo.Commit(p).Tree, path)
					if thereOK == hereOK && there.ID == here.ID {
						changed = false
					}
				}
				if changed {
					kept = append(kept, id)
				}
			}
			commits = kept
		}
		return sendPage(c, len(commits), func(i int) any { return commitView(repo, commits[i], c.u) }, base+"/commits")
	}
	sha, ok := repo.Resolve(strings.Join(s, "/"))
	if !ok || repo.Commit(sha) == nil {
		panic(httpErr(422, "No commit found for SHA: "+strings.Join(s, "/")))
	}
	view := commitView(repo, sha, c.u)
	cm := repo.Commit(sha)
	before := map[string]FlatEntry{}
	if len(cm.Parents) > 0 {
		if pc := repo.Commit(cm.Parents[0]); pc != nil {
			before = repo.Flatten(pc.Tree)
		}
	}
	after := repo.Flatten(cm.Tree)
	set := map[string]bool{}
	for k := range before {
		set[k] = true
	}
	for k := range after {
		set[k] = true
	}
	paths := make([]string, 0, len(set))
	for k := range set {
		paths = append(paths, k)
	}
	sort.Strings(paths)
	files := []any{}
	for _, p := range paths {
		a, aok := before[p]
		b, bok := after[p]
		if aok && bok && a.ID == b.ID {
			continue
		}
		status, sha := "modified", b.ID
		if !aok {
			status = "added"
		} else if !bok {
			status, sha = "removed", a.ID
		}
		files = append(files, Obj{{"filename", p}, {"status", status}, {"sha", sha}, {"additions", 0}, {"deletions", 0}, {"changes", 0}})
	}
	view = append(view, KV{"files", files}, KV{"stats", Obj{{"total", 0}, {"additions", 0}, {"deletions", 0}}})
	return jsonResp(200, view)
}

func gitDataRoutes(c *restCtx, repo *Repo, s []string) *Response {
	m := c.r.Method
	u := c.u
	if len(s) == 0 {
		panic(notFound())
	}
	kind := s[0]
	if kind == "refs" && len(s) == 1 {
		if m == "POST" {
			ref := createRef(repo, c.body["ref"], c.body["sha"])
			return jsonResp(201, refView(repo, ref, repo.Refs[ref], u))
		}
		if m == "GET" {
			list := []any{}
			for _, ref := range repo.SortedRefs() {
				list = append(list, refView(repo, ref[0], ref[1], u))
			}
			return jsonResp(200, list)
		}
	}
	if (kind == "refs" || kind == "ref") && len(s) >= 2 {
		ref := "refs/" + strings.Join(s[1:], "/")
		switch m {
		case "GET":
			if id, ok := repo.Refs[ref]; ok {
				return jsonResp(200, refView(repo, ref, id, u))
			}
			if kind == "refs" {
				list := []any{}
				for _, r := range repo.SortedRefs() {
					if strings.HasPrefix(r[0], ref+"/") {
						list = append(list, refView(repo, r[0], r[1], u))
					}
				}
				if len(list) > 0 {
					return jsonResp(200, list)
				}
			}
			panic(notFound())
		case "PATCH":
			updateRef(repo, ref, c.body["sha"], c.body["force"] == true)
			return jsonResp(200, refView(repo, ref, repo.Refs[ref], u))
		case "DELETE":
			if _, ok := repo.Refs[ref]; !ok {
				panic(httpErr(422, "Reference does not exist"))
			}
			repo.SetRef(ref, "")
			return emptyResp(204)
		}
	}
	if kind == "commits" && len(s) == 2 && m == "GET" {
		if repo.Commit(s[1]) == nil {
			panic(notFound())
		}
		return jsonResp(200, gitCommitView(repo, s[1], u))
	}
	if kind == "blobs" && len(s) == 2 && m == "GET" {
		obj := repo.Get(s[1])
		if obj == nil || obj.Type != "blob" {
			panic(notFound())
		}
		return jsonResp(200, Obj{{"sha", s[1]}, {"node_id", s[1]}, {"size", len(obj.Data)}, {"url", u.API + "/repos/" + repo.FullName() + "/git/blobs/" + s[1]}, {"content", base64.StdEncoding.EncodeToString(obj.Data)}, {"encoding", "base64"}})
	}
	if kind == "trees" && len(s) == 2 && m == "GET" {
		treeID := ""
		if obj := repo.Get(s[1]); obj != nil && obj.Type == "tree" {
			treeID = s[1]
		} else if id, ok := repo.Resolve(s[1]); ok && repo.Commit(id) != nil {
			treeID = repo.Commit(id).Tree
		}
		if treeID == "" {
			panic(notFound())
		}
		rec := c.qDefault("recursive", "")
		_, hasRec := c.q("recursive")
		recursive := hasRec && rec != "0" && rec != "false"
		tree := []any{}
		var walk func(id, prefix string)
		walk = func(id, prefix string) {
			for _, e := range repo.Tree(id) {
				path := e.Name
				if prefix != "" {
					path = prefix + "/" + e.Name
				}
				dir := isTreeMode(e.Mode)
				entry := Obj{{"path", path}}
				typ, mode, kindPath := "blob", e.Mode, "blobs"
				if dir {
					typ, mode, kindPath = "tree", "040000", "trees"
				} else if e.Mode == "160000" {
					typ = "commit"
				}
				entry = append(entry, KV{"mode", mode}, KV{"type", typ}, KV{"sha", e.ID})
				if !dir {
					size := 0
					if obj := repo.Get(e.ID); obj != nil {
						size = len(obj.Data)
					}
					entry = append(entry, KV{"size", size})
				}
				entry = append(entry, KV{"url", u.API + "/repos/" + repo.FullName() + "/git/" + kindPath + "/" + e.ID})
				tree = append(tree, entry)
				if dir && recursive {
					walk(e.ID, path)
				}
			}
		}
		walk(treeID, "")
		return jsonResp(200, Obj{{"sha", treeID}, {"url", u.API + "/repos/" + repo.FullName() + "/git/trees/" + treeID}, {"tree", tree}, {"truncated", false}})
	}
	panic(notFound())
}

func contentRoutes(c *restCtx, repo *Repo, path string) *Response {
	u := c.u
	switch c.r.Method {
	case "GET":
		if repo.IsEmpty() {
			panic(httpErr(404, "This repository is empty."))
		}
		refName := c.qDefault("ref", repo.DefaultBranch)
		tip, ok := repo.Resolve(refName)
		if !ok {
			panic(httpErr(404, "No commit found for the ref "+refName))
		}
		entry, ok := repo.EntryAt(repo.Commit(tip).Tree, path)
		if !ok {
			panic(notFound())
		}
		var parts []string
		for _, p := range strings.Split(path, "/") {
			if p != "" {
				parts = append(parts, p)
			}
		}
		clean := strings.Join(parts, "/")
		if isTreeMode(entry.Mode) {
			list := []any{}
			for _, child := range repo.Tree(entry.ID) {
				childPath := child.Name
				if clean != "" {
					childPath = clean + "/" + child.Name
				}
				size := 0
				if !isTreeMode(child.Mode) {
					if obj := repo.Get(child.ID); obj != nil {
						size = len(obj.Data)
					}
				}
				list = append(list, contentEntryView(repo, childPath, child.Mode, child.ID, size, refName, u))
			}
			return jsonResp(200, list)
		}
		blob := repo.Get(entry.ID)
		view := contentEntryView(repo, clean, entry.Mode, entry.ID, len(blob.Data), refName, u)
		if entry.Mode == "120000" {
			view = append(view, KV{"target", string(blob.Data)})
		}
		view = append(view, KV{"content", base64.StdEncoding.EncodeToString(blob.Data)}, KV{"encoding", "base64"})
		return jsonResp(200, view)
	case "PUT":
		res := putFile(repo, path, c.body)
		content := append(contentEntryView(repo, res.Path, "100644", res.BlobID, res.Size, res.Branch, u), KV{"encoding", "base64"})
		status := 200
		if res.Created {
			status = 201
		}
		return jsonResp(status, Obj{{"content", content}, {"commit", gitCommitView(repo, res.CommitID, u)}})
	case "DELETE":
		commitID, _ := deleteFile(repo, path, c.body)
		return jsonResp(200, Obj{{"content", nil}, {"commit", gitCommitView(repo, commitID, u)}})
	}
	panic(notFound())
}

func labelRoutes(c *restCtx, repo *Repo, s []string, base string) *Response {
	m := c.r.Method
	u := c.u
	if len(s) == 0 {
		if m == "GET" {
			labels := repo.SortedLabels()
			return sendPage(c, len(labels), func(i int) any { return labelView(repo, labels[i], u) }, base+"/labels")
		}
		if m == "POST" {
			return jsonResp(201, labelView(repo, createLabel(repo, c.body), u))
		}
		panic(notFound())
	}
	l := findLabel(repo, strings.Join(s, "/"))
	if l == nil {
		panic(notFound())
	}
	switch m {
	case "GET":
		return jsonResp(200, labelView(repo, l, u))
	case "PATCH":
		newName := l.Name
		if n, ok := c.body["new_name"].(string); ok {
			newName = n
		} else if n, ok := c.body["name"].(string); ok {
			newName = n
		}
		if !strings.EqualFold(newName, l.Name) {
			if findLabel(repo, newName) != nil {
				panic(httpErr(422, "Validation Failed"))
			}
			oldKey := strings.ToLower(l.Name)
			delete(repo.Labels, oldKey)
			repo.Labels[strings.ToLower(newName)] = l
			for _, i := range repo.Issues {
				for k := range i.Labels {
					if i.Labels[k] == oldKey {
						i.Labels[k] = strings.ToLower(newName)
					}
				}
			}
		}
		l.Name = newName
		if col, ok := c.body["color"].(string); ok {
			l.Color = strings.TrimPrefix(col, "#")
		}
		if v, ok := c.body["description"]; ok {
			if d, isStr := v.(string); isStr {
				l.Description = &d
			} else {
				l.Description = nil
			}
		}
		return jsonResp(200, labelView(repo, l, u))
	case "DELETE":
		key := strings.ToLower(l.Name)
		delete(repo.Labels, key)
		for _, i := range repo.Issues {
			i.Labels = removeString(i.Labels, key)
		}
		return emptyResp(204)
	}
	panic(notFound())
}

func removeString(list []string, v string) []string {
	out := []string{}
	for _, x := range list {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

func parseISO(s string) float64 {
	t, err := timeParse(s)
	if err != nil {
		return math.NaN()
	}
	return t
}

func sortIssues(list []*Issue, by, direction string) []*Issue {
	key := func(i *Issue) float64 {
		switch by {
		case "comments":
			return float64(len(i.Comments))
		case "updated":
			return parseISO(i.UpdatedAt)
		}
		return float64(i.Seq)
	}
	sorted := append([]*Issue(nil), list...)
	sort.SliceStable(sorted, func(a, b int) bool {
		ka, kb := key(sorted[a]), key(sorted[b])
		if ka != kb {
			return ka < kb
		}
		return sorted[a].Number < sorted[b].Number
	})
	if direction != "asc" {
		for i, j := 0, len(sorted)-1; i < j; i, j = i+1, j-1 {
			sorted[i], sorted[j] = sorted[j], sorted[i]
		}
	}
	return sorted
}

func issueRoutes(c *restCtx, repo *Repo, s []string, base string) *Response {
	m := c.r.Method
	u := c.u
	if len(s) == 0 {
		if m == "POST" {
			issue := createIssue(repo, c.body)
			return jsonResp(201, issueView(repo, issue, u)).Add("location", fmt.Sprintf("%s%s/issues/%d", u.API, base, issue.Number))
		}
		if m == "GET" {
			state := c.qDefault("state", "open")
			var labels []string
			for _, l := range strings.Split(c.qDefault("labels", ""), ",") {
				if l = strings.ToLower(strings.TrimSpace(l)); l != "" {
					labels = append(labels, l)
				}
			}
			var list []*Issue
			for _, i := range repo.SortedIssues() {
				if i.IsPull || (state != "all" && i.State != state) {
					continue
				}
				ok := true
				for _, l := range labels {
					if !containsString(i.Labels, l) {
						ok = false
					}
				}
				if ok {
					list = append(list, i)
				}
			}
			list = sortIssues(list, c.qDefault("sort", "created"), c.qDefault("direction", "desc"))
			return sendPage(c, len(list), func(k int) any { return issueView(repo, list[k], u) }, base+"/issues")
		}
		panic(notFound())
	}
	if s[0] == "comments" {
		if len(s) == 1 && m == "GET" {
			type pair struct {
				i *Issue
				c *Comment
			}
			var all []pair
			for _, i := range repo.SortedIssues() {
				for _, cm := range i.Comments {
					all = append(all, pair{i, cm})
				}
			}
			sort.SliceStable(all, func(a, b int) bool { return all[a].c.ID < all[b].c.ID })
			return sendPage(c, len(all), func(k int) any { return commentView(repo, all[k].i, all[k].c, u) }, base+"/issues/comments")
		}
		if len(s) != 2 {
			panic(notFound())
		}
		issue, comment := findComment(repo, intParam(s[1]))
		if comment == nil {
			panic(notFound())
		}
		switch m {
		case "GET":
			return jsonResp(200, commentView(repo, issue, comment, u))
		case "PATCH":
			b, ok := c.body["body"].(string)
			if !ok || b == "" {
				panic(httpErr(422, "Validation Failed"))
			}
			comment.Body = b
			_, comment.UpdatedAt = tick()
			return jsonResp(200, commentView(repo, issue, comment, u))
		case "DELETE":
			var kept []*Comment
			for _, x := range issue.Comments {
				if x != comment {
					kept = append(kept, x)
				}
			}
			issue.Comments = kept
			return emptyResp(204)
		}
		panic(notFound())
	}
	issue := requireIssue(repo, intParam(s[0]))
	if len(s) == 1 {
		switch m {
		case "GET":
			return jsonResp(200, issueView(repo, issue, u))
		case "PATCH":
			return jsonResp(200, issueView(repo, updateIssue(repo, issue, c.body), u))
		}
		panic(notFound())
	}
	if s[1] == "comments" && len(s) == 2 {
		if m == "GET" {
			return sendPage(c, len(issue.Comments), func(k int) any { return commentView(repo, issue, issue.Comments[k], u) }, fmt.Sprintf("%s/issues/%d/comments", base, issue.Number))
		}
		if m == "POST" {
			return jsonResp(201, commentView(repo, issue, addComment(issue, c.body["body"]), u))
		}
	}
	if s[1] == "labels" && len(s) == 2 {
		switch m {
		case "GET":
			return jsonResp(200, labelsOf(repo, issue.Labels, u))
		case "POST", "PUT":
			list, _ := c.body["labels"].([]any)
			keys := labelKeys(repo, list)
			if m == "PUT" {
				issue.Labels = keys
			} else {
				for _, k := range keys {
					if !containsString(issue.Labels, k) {
						issue.Labels = append(issue.Labels, k)
					}
				}
			}
			return jsonResp(200, labelsOf(repo, issue.Labels, u))
		case "DELETE":
			issue.Labels = []string{}
			return emptyResp(204)
		}
	}
	if s[1] == "labels" && len(s) == 3 && m == "DELETE" {
		key := strings.ToLower(s[2])
		if !containsString(issue.Labels, key) {
			panic(notFound())
		}
		issue.Labels = removeString(issue.Labels, key)
		return jsonResp(200, labelsOf(repo, issue.Labels, u))
	}
	panic(notFound())
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func pullRoutes(c *restCtx, repo *Repo, s []string, base string) *Response {
	m := c.r.Method
	u := c.u
	if len(s) == 0 {
		if m == "POST" {
			p := createPull(repo, c.body)
			return jsonResp(201, pullView(repo, p, u, true)).Add("location", fmt.Sprintf("%s%s/pulls/%d", u.API, base, p.Number))
		}
		if m == "GET" {
			state := c.qDefault("state", "open")
			headFilter, hasHead := c.q("head")
			baseFilter, hasBase := c.q("base")
			var list []*Pull
			for _, p := range repo.SortedPulls() {
				if state != "all" && p.Issue.State != state {
					continue
				}
				if hasHead && repo.Owner+":"+p.HeadRef != headFilter && p.HeadRef != headFilter {
					continue
				}
				if hasBase && p.BaseRef != baseFilter {
					continue
				}
				list = append(list, p)
			}
			if c.qDefault("direction", "desc") == "desc" {
				for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
					list[i], list[j] = list[j], list[i]
				}
			}
			return sendPage(c, len(list), func(k int) any { return pullView(repo, list[k], u, false) }, base+"/pulls")
		}
		panic(notFound())
	}
	p := requirePull(repo, intParam(s[0]))
	if len(s) == 1 {
		switch m {
		case "GET":
			return jsonResp(200, pullView(repo, p, u, true))
		case "PATCH":
			return jsonResp(200, pullView(repo, updatePull(repo, p, c.body), u, true))
		}
		panic(notFound())
	}
	headSha := pullHeadSha(repo, p)
	if headSha == zeroID {
		headSha = ""
	}
	switch {
	case s[1] == "files" && len(s) == 2 && m == "GET":
		files := pullFiles(repo, p)
		return sendPage(c, len(files), func(k int) any {
			f := files[k]
			return Obj{
				{"sha", f.Sha}, {"filename", f.Filename}, {"status", f.Status}, {"additions", f.Additions}, {"deletions", f.Deletions}, {"changes", f.Changes},
				{"blob_url", u.Web + "/" + repo.FullName() + "/blob/" + headSha + "/" + f.Filename},
				{"raw_url", u.Web + "/" + repo.FullName() + "/raw/" + headSha + "/" + f.Filename},
				{"contents_url", u.API + base + "/contents/" + f.Filename + "?ref=" + headSha},
			}
		}, fmt.Sprintf("%s/pulls/%d/files", base, p.Number))
	case s[1] == "commits" && len(s) == 2 && m == "GET":
		commits := pullCommits(repo, p)
		return sendPage(c, len(commits), func(k int) any { return commitView(repo, commits[k], u) }, fmt.Sprintf("%s/pulls/%d/commits", base, p.Number))
	case s[1] == "merge" && len(s) == 2:
		if m == "GET" {
			if p.Merged {
				return emptyResp(204)
			}
			panic(notFound())
		}
		if m == "PUT" {
			method, _ := c.body["merge_method"].(string)
			title, _ := c.body["commit_title"].(string)
			message, _ := c.body["commit_message"].(string)
			sha, _ := c.body["sha"].(string)
			commit, msg := mergePull(repo, p, method, title, message, sha)
			return jsonResp(200, Obj{{"sha", commit}, {"merged", true}, {"message", msg}})
		}
	}
	panic(notFound())
}
