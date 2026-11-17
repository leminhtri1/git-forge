package main

// GitHub REST v3 object shapes (the Go twin of src/rest/views.ts), as ordered objects.

import (
	"fmt"
	"net/url"
	"strings"
)

type Urls struct {
	API string
	Web string
}

func urlsOf(r *Request) Urls {
	origin := originOf(r)
	return Urls{API: origin + "/api/v3", Web: origin}
}

func nullable(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

var userViewCache = map[string]Obj{}

func userView(u Urls) Obj {
	if v, ok := userViewCache[u.API]; ok {
		return v
	}
	login := config.Viewer.Login
	v := Obj{
		{"login", login},
		{"id", config.Viewer.ID},
		{"node_id", nodeID("User", config.Viewer.ID)},
		{"avatar_url", u.Web + "/avatars/" + login},
		{"gravatar_id", ""},
		{"url", u.API + "/users/" + login},
		{"html_url", u.Web + "/" + login},
		{"repos_url", u.API + "/users/" + login + "/repos"},
		{"type", "User"},
		{"site_admin", false},
	}
	if len(userViewCache) > 64 {
		userViewCache = map[string]Obj{}
	}
	userViewCache[u.API] = v
	return v
}

func fullUserView(u Urls) Obj {
	v := append(Obj{}, userView(u)...)
	return append(v, Obj{
		{"name", config.Viewer.Name},
		{"email", config.Viewer.Email},
		{"company", nil},
		{"blog", ""},
		{"location", nil},
		{"bio", nil},
		{"public_repos", 0},
		{"followers", 0},
		{"following", 0},
		{"created_at", "2025-01-01T00:00:00Z"},
		{"updated_at", "2025-01-01T00:00:00Z"},
	}...)
}

func repoView(r *Repo, u Urls) Obj {
	api := u.API + "/repos/" + r.FullName()
	open := 0
	for _, i := range r.Issues {
		if i.State == "open" {
			open++
		}
	}
	visibility := "public"
	if r.IsPrivate {
		visibility = "private"
	}
	return Obj{
		{"id", r.ID},
		{"node_id", nodeID("Repository", r.ID)},
		{"name", r.Name},
		{"full_name", r.FullName()},
		{"private", r.IsPrivate},
		{"owner", userView(u)},
		{"html_url", u.Web + "/" + r.FullName()},
		{"description", nullable(r.Description)},
		{"fork", false},
		{"url", api},
		{"issues_url", api + "/issues{/number}"},
		{"pulls_url", api + "/pulls{/number}"},
		{"contents_url", api + "/contents/{+path}"},
		{"branches_url", api + "/branches{/branch}"},
		{"commits_url", api + "/commits{/sha}"},
		{"labels_url", api + "/labels{/name}"},
		{"git_url", u.Web + "/" + r.FullName() + ".git"},
		{"clone_url", u.Web + "/" + r.FullName() + ".git"},
		{"ssh_url", "git@forge.local:" + r.FullName() + ".git"},
		{"homepage", nullable(r.Homepage)},
		{"size", 0},
		{"stargazers_count", 0},
		{"watchers_count", 0},
		{"watchers", 0},
		{"language", nil},
		{"has_issues", true},
		{"has_projects", true},
		{"has_wiki", false},
		{"has_pages", false},
		{"has_downloads", true},
		{"has_discussions", false},
		{"archived", false},
		{"disabled", false},
		{"visibility", visibility},
		{"forks_count", 0},
		{"forks", 0},
		{"open_issues_count", open},
		{"open_issues", open},
		{"topics", []any{}},
		{"is_template", false},
		{"default_branch", r.DefaultBranch},
		{"created_at", r.CreatedAt},
		{"updated_at", r.UpdatedAt},
		{"pushed_at", nullable(r.PushedAt)},
		{"permissions", Obj{{"admin", true}, {"maintain", true}, {"push", true}, {"triage", true}, {"pull", true}}},
		{"allow_squash_merge", true},
		{"allow_merge_commit", true},
		{"allow_rebase_merge", true},
		{"delete_branch_on_merge", false},
	}
}

func labelView(r *Repo, l *Label, u Urls) Obj {
	return Obj{
		{"id", l.ID},
		{"node_id", nodeID("Label", l.ID)},
		{"url", u.API + "/repos/" + r.FullName() + "/labels/" + url.PathEscape(l.Name)},
		{"name", l.Name},
		{"color", l.Color},
		{"default", false},
		{"description", nullable(l.Description)},
	}
}

func labelsOf(r *Repo, keys []string, u Urls) []any {
	out := []any{}
	for _, k := range keys {
		if l := r.Labels[k]; l != nil {
			out = append(out, labelView(r, l, u))
		}
	}
	return out
}

func issueView(r *Repo, i *Issue, u Urls) Obj {
	api := u.API + "/repos/" + r.FullName()
	n := fmt.Sprint(i.Number)
	node := nodeID("Issue", i.ID)
	kindPath := "issues"
	if i.IsPull {
		kindPath = "pull"
		pid := i.ID
		if p := r.Pulls[i.Number]; p != nil {
			pid = p.ID
		}
		node = nodeID("PullRequest", pid)
	}
	var closedBy any
	if i.State == "closed" {
		closedBy = userView(u)
	}
	v := Obj{
		{"id", i.ID},
		{"node_id", node},
		{"url", api + "/issues/" + n},
		{"repository_url", api},
		{"labels_url", api + "/issues/" + n + "/labels{/name}"},
		{"comments_url", api + "/issues/" + n + "/comments"},
		{"html_url", u.Web + "/" + r.FullName() + "/" + kindPath + "/" + n},
		{"number", i.Number},
		{"state", i.State},
		{"state_reason", nullable(i.StateReason)},
		{"title", i.Title},
		{"body", nullable(i.Body)},
		{"user", userView(u)},
		{"labels", labelsOf(r, i.Labels, u)},
		{"assignee", nil},
		{"assignees", []any{}},
		{"milestone", nil},
		{"locked", false},
		{"active_lock_reason", nil},
		{"comments", len(i.Comments)},
		{"created_at", i.CreatedAt},
		{"updated_at", i.UpdatedAt},
		{"closed_at", nullable(i.ClosedAt)},
		{"closed_by", closedBy},
		{"author_association", "OWNER"},
		{"reactions", Obj{{"url", api + "/issues/" + n + "/reactions"}, {"total_count", 0}, {"+1", 0}, {"-1", 0}, {"laugh", 0}, {"hooray", 0}, {"confused", 0}, {"heart", 0}, {"rocket", 0}, {"eyes", 0}}},
	}
	if i.IsPull {
		var mergedAt any
		if p := r.Pulls[i.Number]; p != nil {
			mergedAt = nullable(p.MergedAt)
		}
		web := u.Web + "/" + r.FullName() + "/pull/" + n
		v = append(v, KV{"pull_request", Obj{{"url", api + "/pulls/" + n}, {"html_url", web}, {"diff_url", web + ".diff"}, {"patch_url", web + ".patch"}, {"merged_at", mergedAt}}})
	}
	return v
}

func commentView(r *Repo, i *Issue, c *Comment, u Urls) Obj {
	api := u.API + "/repos/" + r.FullName()
	return Obj{
		{"id", c.ID},
		{"node_id", nodeID("IssueComment", c.ID)},
		{"url", fmt.Sprintf("%s/issues/comments/%d", api, c.ID)},
		{"html_url", fmt.Sprintf("%s/%s/issues/%d#issuecomment-%d", u.Web, r.FullName(), i.Number, c.ID)},
		{"issue_url", fmt.Sprintf("%s/issues/%d", api, i.Number)},
		{"body", c.Body},
		{"user", userView(u)},
		{"created_at", c.CreatedAt},
		{"updated_at", c.UpdatedAt},
		{"author_association", "OWNER"},
	}
}

func branchSide(r *Repo, ref, sha string, u Urls) Obj {
	return Obj{{"label", r.Owner + ":" + ref}, {"ref", ref}, {"sha", sha}, {"user", userView(u)}, {"repo", repoView(r, u)}}
}

func pullView(r *Repo, p *Pull, u Urls, detailed bool) Obj {
	api := u.API + "/repos/" + r.FullName()
	n := fmt.Sprint(p.Number)
	web := u.Web + "/" + r.FullName() + "/pull/" + n
	i := p.Issue
	v := Obj{
		{"id", p.ID},
		{"node_id", nodeID("PullRequest", p.ID)},
		{"url", api + "/pulls/" + n},
		{"html_url", web},
		{"diff_url", web + ".diff"},
		{"patch_url", web + ".patch"},
		{"issue_url", api + "/issues/" + n},
		{"commits_url", api + "/pulls/" + n + "/commits"},
		{"comments_url", api + "/issues/" + n + "/comments"},
		{"number", p.Number},
		{"state", i.State},
		{"locked", false},
		{"title", i.Title},
		{"body", nullable(i.Body)},
		{"user", userView(u)},
		{"labels", labelsOf(r, i.Labels, u)},
		{"milestone", nil},
		{"assignee", nil},
		{"assignees", []any{}},
		{"requested_reviewers", []any{}},
		{"created_at", i.CreatedAt},
		{"updated_at", i.UpdatedAt},
		{"closed_at", nullable(i.ClosedAt)},
		{"merged_at", nullable(p.MergedAt)},
		{"merge_commit_sha", nullable(p.MergeCommitSha)},
		{"draft", p.Draft},
		{"head", branchSide(r, p.HeadRef, pullHeadSha(r, p), u)},
		{"base", branchSide(r, p.BaseRef, pullBaseSha(r, p), u)},
		{"author_association", "OWNER"},
		{"auto_merge", nil},
		{"active_lock_reason", nil},
	}
	v = append(v, KV{"merged", p.Merged})
	if detailed {
		files := pullFiles(r, p)
		m := pullMergeable(r, p)
		var mergeable any
		state := "unknown"
		if m == 1 {
			mergeable, state = true, "clean"
		} else if m == 0 {
			mergeable, state = false, "dirty"
		}
		if p.Merged {
			state = "unknown"
		}
		var mergedBy any
		if p.Merged {
			mergedBy = userView(u)
		}
		adds, dels := 0, 0
		for _, f := range files {
			adds += f.Additions
			dels += f.Deletions
		}
		v = append(v, Obj{
			{"mergeable", mergeable},
			{"rebaseable", mergeable},
			{"mergeable_state", state},
			{"merged_by", mergedBy},
			{"comments", len(i.Comments)},
			{"review_comments", 0},
			{"maintainer_can_modify", false},
			{"commits", len(pullCommits(r, p))},
			{"additions", adds},
			{"deletions", dels},
			{"changed_files", len(files)},
		}...)
	}
	return v
}

func stripOneNewline(s string) string { return strings.TrimSuffix(s, "\n") }

func commitView(r *Repo, sha string, u Urls) Obj {
	c := r.Commit(sha)
	api := u.API + "/repos/" + r.FullName()
	a := parseSignature(c.Author)
	cm := parseSignature(c.Committer)
	mine := func(email string) any {
		if email == config.Viewer.Email {
			return userView(u)
		}
		return nil
	}
	parents := []any{}
	for _, p := range c.Parents {
		parents = append(parents, Obj{{"sha", p}, {"url", api + "/commits/" + p}, {"html_url", u.Web + "/" + r.FullName() + "/commit/" + p}})
	}
	return Obj{
		{"sha", sha},
		{"node_id", nodeID("Commit", sha)},
		{"url", api + "/commits/" + sha},
		{"html_url", u.Web + "/" + r.FullName() + "/commit/" + sha},
		{"comments_url", api + "/commits/" + sha + "/comments"},
		{"commit", Obj{
			{"url", api + "/git/commits/" + sha},
			{"author", Obj{{"name", a.Name}, {"email", a.Email}, {"date", signatureDate(c.Author)}}},
			{"committer", Obj{{"name", cm.Name}, {"email", cm.Email}, {"date", signatureDate(c.Committer)}}},
			{"message", stripOneNewline(c.Message)},
			{"tree", Obj{{"sha", c.Tree}, {"url", api + "/git/trees/" + c.Tree}}},
			{"comment_count", 0},
			{"verification", Obj{{"verified", false}, {"reason", "unsigned"}, {"signature", nil}, {"payload", nil}}},
		}},
		{"author", mine(a.Email)},
		{"committer", mine(cm.Email)},
		{"parents", parents},
	}
}

func gitCommitView(r *Repo, sha string, u Urls) Obj {
	c := r.Commit(sha)
	api := u.API + "/repos/" + r.FullName()
	a := parseSignature(c.Author)
	cm := parseSignature(c.Committer)
	parents := []any{}
	for _, p := range c.Parents {
		parents = append(parents, Obj{{"sha", p}, {"url", api + "/git/commits/" + p}, {"html_url", u.Web + "/" + r.FullName() + "/commit/" + p}})
	}
	return Obj{
		{"sha", sha},
		{"node_id", nodeID("Commit", sha)},
		{"url", api + "/git/commits/" + sha},
		{"html_url", u.Web + "/" + r.FullName() + "/commit/" + sha},
		{"author", Obj{{"name", a.Name}, {"email", a.Email}, {"date", signatureDate(c.Author)}}},
		{"committer", Obj{{"name", cm.Name}, {"email", cm.Email}, {"date", signatureDate(c.Committer)}}},
		{"message", stripOneNewline(c.Message)},
		{"tree", Obj{{"sha", c.Tree}, {"url", api + "/git/trees/" + c.Tree}}},
		{"parents", parents},
		{"verification", Obj{{"verified", false}, {"reason", "unsigned"}, {"signature", nil}, {"payload", nil}}},
	}
}

func entryType(mode string) string {
	switch {
	case isTreeMode(mode):
		return "dir"
	case mode == "120000":
		return "symlink"
	case mode == "160000":
		return "submodule"
	}
	return "file"
}

func contentEntryView(r *Repo, path, mode, sha string, size int, ref string, u Urls) Obj {
	api := u.API + "/repos/" + r.FullName()
	typ := entryType(mode)
	name := path[strings.LastIndexByte(path, '/')+1:]
	self := api + "/contents/" + path + "?ref=" + url.QueryEscape(ref)
	kind, htmlKind := "blobs", "blob"
	if typ == "dir" {
		kind, htmlKind = "trees", "tree"
	}
	git := api + "/git/" + kind + "/" + sha
	html := u.Web + "/" + r.FullName() + "/" + htmlKind + "/" + ref + "/" + path
	var download any
	if typ == "file" {
		download = u.Web + "/" + r.FullName() + "/raw/" + ref + "/" + path
	}
	return Obj{
		{"type", typ},
		{"size", size},
		{"name", name},
		{"path", path},
		{"sha", sha},
		{"url", self},
		{"git_url", git},
		{"html_url", html},
		{"download_url", download},
		{"_links", Obj{{"self", self}, {"git", git}, {"html", html}}},
	}
}

func refView(r *Repo, ref, sha string, u Urls) Obj {
	typ := "commit"
	if obj := r.Get(sha); obj != nil {
		typ = obj.Type
	}
	return Obj{
		{"ref", ref},
		{"node_id", nodeID("Ref", fmt.Sprintf("%d:%s", r.ID, ref))},
		{"url", u.API + "/repos/" + r.FullName() + "/git/" + ref},
		{"object", Obj{{"sha", sha}, {"type", typ}, {"url", u.API + "/repos/" + r.FullName() + "/git/" + typ + "s/" + sha}}},
	}
}

func branchView(r *Repo, name, sha string, u Urls, detailed bool) Obj {
	if !detailed {
		return Obj{{"name", name}, {"commit", Obj{{"sha", sha}, {"url", u.API + "/repos/" + r.FullName() + "/commits/" + sha}}}, {"protected", false}}
	}
	return Obj{
		{"name", name},
		{"commit", commitView(r, sha, u)},
		{"protected", false},
		{"_links", Obj{{"self", u.API + "/repos/" + r.FullName() + "/branches/" + name}, {"html", u.Web + "/" + r.FullName() + "/tree/" + name}}},
		{"protection", Obj{{"enabled", false}, {"required_status_checks", Obj{{"enforcement_level", "off"}, {"contexts", []any{}}, {"checks", []any{}}}}}},
		{"protection_url", u.API + "/repos/" + r.FullName() + "/branches/" + name + "/protection"},
	}
}
