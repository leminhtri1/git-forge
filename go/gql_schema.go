package main

// The GitHub GraphQL v4 subset this forge serves (the Go twin of src/graphql/schema.ts): the queries and
// mutations gh and github-mcp-server send. Runtime values are small structs over the store.

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------- runtime values

type repoVal struct{ repo *Repo }
type issueVal struct {
	repo  *Repo
	issue *Issue
}
type pullVal struct {
	repo *Repo
	pull *Pull
}
type commentVal struct {
	repo    *Repo
	issue   *Issue
	comment *Comment
}
type labelVal struct {
	repo  *Repo
	label *Label
}
type userVal struct{}
type refVal struct {
	repo                 *Repo
	name, prefix, target string
}
type commitVal struct {
	repo *Repo
	oid  string
	typ  string
}

func (repoVal) TypeName() string    { return "Repository" }
func (issueVal) TypeName() string   { return "Issue" }
func (pullVal) TypeName() string    { return "PullRequest" }
func (commentVal) TypeName() string { return "IssueComment" }
func (labelVal) TypeName() string   { return "Label" }
func (userVal) TypeName() string    { return "User" }
func (refVal) TypeName() string     { return "Ref" }
func (c commitVal) TypeName() string {
	if c.typ != "" {
		return c.typ
	}
	return "Commit"
}

var theUser = userVal{}

func webURL(path string) string { return "http://forge.local/" + path }

// ---------------------------------------------------------------- connections

func cursorFor(i int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("cursor:v2:%d", i)))
}

var cursorRE = regexp.MustCompile(`^cursor:v2:(\d+)$`)

func indexOfCursor(c any) (int, bool) {
	s, ok := c.(string)
	if !ok || s == "" {
		return 0, false
	}
	raw, _ := base64.StdEncoding.DecodeString(s)
	m := cursorRE.FindStringSubmatch(string(raw))
	if m == nil {
		panic(fieldErr(fmt.Sprintf("`%s` does not appear to be a valid cursor.", s), "INVALID_CURSOR_ARGUMENTS"))
	}
	n, _ := strconv.Atoi(m[1])
	return n, true
}

var connectionArgs = []ArgDef{arg("first", "Int"), arg("last", "Int"), arg("after", "String"), arg("before", "String")}

func withConnArgs(extra ...ArgDef) []ArgDef {
	return append(append([]ArgDef(nil), connectionArgs...), extra...)
}

func numArg(args map[string]any, name string) (int, bool) {
	n, ok := args[name].(float64)
	return int(n), ok
}

func connection(items []any, args map[string]any) Obj {
	start, end := 0, len(items)
	if after, ok := indexOfCursor(args["after"]); ok && after+1 > start {
		start = after + 1
	}
	if before, ok := indexOfCursor(args["before"]); ok && before < end {
		end = before
	}
	first, hasFirst := numArg(args, "first")
	last, hasLast := numArg(args, "last")
	if hasFirst {
		if first < 0 {
			panic(fieldErr("`first` on a connection cannot be less than zero.", "INVALID_PAGINATION"))
		}
		if start+first < end {
			end = start + first
		}
	}
	if hasLast {
		if last < 0 {
			panic(fieldErr("`last` on a connection cannot be less than zero.", "INVALID_PAGINATION"))
		}
		if end-last > start {
			start = end - last
		}
	}
	if !hasFirst && !hasLast && start+100 < end {
		end = start + 100
	}
	if start > end {
		start = end
	}
	slice := items[start:end]
	nodes := make([]any, len(slice))
	edges := make([]any, len(slice))
	for i, n := range slice {
		nodes[i] = n
		edges[i] = Obj{{"node", n}, {"cursor", cursorFor(start + i)}}
	}
	var startCursor, endCursor any
	if len(slice) > 0 {
		startCursor, endCursor = cursorFor(start), cursorFor(end-1)
	}
	return Obj{
		{"totalCount", len(items)},
		{"nodes", nodes},
		{"edges", edges},
		{"pageInfo", Obj{{"hasNextPage", end < len(items)}, {"hasPreviousPage", start > 0}, {"startCursor", startCursor}, {"endCursor", endCursor}}},
	}
}

func connectionTypes(name, node string) []*TypeDef {
	return []*TypeDef{
		{Kind: "OBJECT", Name: name + "Connection", Fields: []*FieldDef{
			f("totalCount", "Int!", nil), f("nodes", "["+node+"]", nil), f("edges", "["+name+"Edge]", nil), f("pageInfo", "PageInfo!", nil),
		}},
		{Kind: "OBJECT", Name: name + "Edge", Fields: []*FieldDef{f("node", node, nil), f("cursor", "String!", nil)}},
	}
}

// ---------------------------------------------------------------- lookups

func nodeByID(id any) any {
	s, ok := id.(string)
	if !ok {
		return nil
	}
	kind, raw, ok := decodeNodeID(s)
	if !ok {
		return nil
	}
	n, _ := strconv.Atoi(raw)
	switch kind {
	case "Repository":
		if r := store.ByID(n); r != nil {
			return repoVal{r}
		}
	case "User":
		if n == config.Viewer.ID {
			return theUser
		}
	case "PullRequest":
		for _, r := range store.All() {
			for _, p := range r.Pulls {
				if p.ID == n {
					return pullVal{r, p}
				}
			}
		}
	case "Issue":
		for _, r := range store.All() {
			for _, i := range r.Issues {
				if i.ID == n && !i.IsPull {
					return issueVal{r, i}
				}
			}
		}
	case "IssueComment":
		for _, r := range store.All() {
			for _, i := range r.SortedIssues() {
				for _, c := range i.Comments {
					if c.ID == n {
						return commentVal{r, i, c}
					}
				}
			}
		}
	case "Label":
		for _, r := range store.All() {
			for _, l := range r.Labels {
				if l.ID == n {
					return labelVal{r, l}
				}
			}
		}
	}
	return nil
}

func notFoundNode(id any) *FieldError {
	return fieldErr(fmt.Sprintf("Could not resolve to a node with the global id of '%v'", id), "NOT_FOUND")
}

func requireRepoNode(id any) *Repo {
	if v, ok := nodeByID(id).(repoVal); ok {
		return v.repo
	}
	panic(notFoundNode(id))
}

func requireIssueNode(id any) issueVal {
	if v, ok := nodeByID(id).(issueVal); ok {
		return v
	}
	panic(notFoundNode(id))
}

func requirePullNode(id any) pullVal {
	if v, ok := nodeByID(id).(pullVal); ok {
		return v
	}
	panic(notFoundNode(id))
}

func labelNames(r *Repo, ids any) ([]any, bool) {
	list, ok := ids.([]any)
	if !ok {
		return nil, false
	}
	out := []any{}
	for _, id := range list {
		v, isLabel := nodeByID(id).(labelVal)
		if !isLabel || v.repo != r {
			panic(notFoundNode(id))
		}
		out = append(out, v.label.Name)
	}
	return out, true
}

// ---------------------------------------------------------------- helpers

func clientMutationID(in map[string]any) any {
	if v, ok := in["clientMutationId"]; ok {
		return v
	}
	return nil
}

func inputOf(args map[string]any) map[string]any {
	in, _ := args["input"].(map[string]any)
	if in == nil {
		in = map[string]any{}
	}
	return in
}

func issueOf(v any) (*Repo, *Issue) {
	switch x := v.(type) {
	case issueVal:
		return x.repo, x.issue
	case pullVal:
		return x.repo, x.pull.Issue
	}
	return nil, nil
}

func pullState(p *Pull) string {
	if p.Merged {
		return "MERGED"
	}
	if p.Issue.State == "open" {
		return "OPEN"
	}
	return "CLOSED"
}

func gqlSortIssues(list []*Issue, orderBy any) []*Issue {
	field, direction := "CREATED_AT", "ASC"
	if m, ok := orderBy.(map[string]any); ok {
		if s, isStr := m["field"].(string); isStr {
			field = s
		}
		if s, isStr := m["direction"].(string); isStr {
			direction = s
		}
	}
	key := func(i *Issue) float64 {
		switch field {
		case "COMMENTS":
			return float64(len(i.Comments))
		case "UPDATED_AT":
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
	if direction == "DESC" {
		for i, j := 0, len(sorted)-1; i < j; i, j = i+1, j-1 {
			sorted[i], sorted[j] = sorted[j], sorted[i]
		}
	}
	return sorted
}

func stringList(v any) []string {
	list, _ := v.([]any)
	var out []string
	for _, x := range list {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func commonIssueFields(pick func(any) *Issue) []*FieldDef {
	return []*FieldDef{
		f("number", "Int!", func(v any, _ map[string]any) any { return pick(v).Number }),
		f("title", "String!", func(v any, _ map[string]any) any { return pick(v).Title }),
		f("body", "String!", func(v any, _ map[string]any) any {
			if b := pick(v).Body; b != nil {
				return *b
			}
			return ""
		}),
		f("bodyText", "String!", func(v any, _ map[string]any) any {
			if b := pick(v).Body; b != nil {
				return *b
			}
			return ""
		}),
		f("closed", "Boolean!", func(v any, _ map[string]any) any { return pick(v).State == "closed" }),
		f("closedAt", "DateTime", func(v any, _ map[string]any) any { return nullable(pick(v).ClosedAt) }),
		f("createdAt", "DateTime!", func(v any, _ map[string]any) any { return pick(v).CreatedAt }),
		f("updatedAt", "DateTime!", func(v any, _ map[string]any) any { return pick(v).UpdatedAt }),
		f("locked", "Boolean!", constant(false)),
		f("author", "Actor", constant(theUser)),
		f("repository", "Repository!", func(v any, _ map[string]any) any { r, _ := issueOf(v); return repoVal{r} }),
		f("labels", "LabelConnection", func(v any, a map[string]any) any {
			r, i := issueOf(v)
			items := []any{}
			for _, k := range i.Labels {
				if l := r.Labels[k]; l != nil {
					items = append(items, labelVal{r, l})
				}
			}
			return connection(items, a)
		}, connectionArgs...),
		f("comments", "IssueCommentConnection!", func(v any, a map[string]any) any {
			r, i := issueOf(v)
			items := []any{}
			for _, c := range i.Comments {
				items = append(items, commentVal{r, i, c})
			}
			return connection(items, a)
		}, connectionArgs...),
		f("assignees", "UserConnection!", func(_ any, a map[string]any) any { return connection([]any{}, a) }, connectionArgs...),
		f("milestone", "Milestone", constant(nil)),
		f("viewerCanUpdate", "Boolean!", constant(true)),
	}
}

func payload(name string, fields ...[2]string) *TypeDef {
	t := &TypeDef{Kind: "OBJECT", Name: name, Fields: []*FieldDef{f("clientMutationId", "String", nil)}}
	for _, fl := range fields {
		t.Fields = append(t.Fields, f(fl[0], fl[1], nil))
	}
	return t
}

func enumDef(name string, values ...string) *TypeDef {
	return &TypeDef{Kind: "ENUM", Name: name, Values: values}
}

func inputDef(name string, fields ...ArgDef) *TypeDef {
	return &TypeDef{Kind: "INPUT_OBJECT", Name: name, InputFields: fields}
}

func objDef(name string, interfaces []string, fields ...*FieldDef) *TypeDef {
	return &TypeDef{Kind: "OBJECT", Name: name, Interfaces: interfaces, Fields: fields}
}

func flatten(groups ...[]*FieldDef) []*FieldDef {
	var out []*FieldDef
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

func viewerLoginMatches(args map[string]any) bool {
	login, _ := args["login"].(string)
	return strings.EqualFold(login, config.Viewer.Login)
}

// ---------------------------------------------------------------- the schema

func buildGitHubSchema() *Schema {
	types := []*TypeDef{
		{Kind: "SCALAR", Name: "DateTime"}, {Kind: "SCALAR", Name: "URI"}, {Kind: "SCALAR", Name: "GitObjectID"}, {Kind: "SCALAR", Name: "HTML"},
		enumDef("IssueState", "OPEN", "CLOSED"),
		enumDef("IssueStateReason", "COMPLETED", "NOT_PLANNED", "REOPENED", "DUPLICATE"),
		enumDef("IssueClosedStateReason", "COMPLETED", "NOT_PLANNED", "DUPLICATE"),
		enumDef("PullRequestState", "OPEN", "CLOSED", "MERGED"),
		enumDef("MergeableState", "MERGEABLE", "CONFLICTING", "UNKNOWN"),
		enumDef("PullRequestMergeMethod", "MERGE", "SQUASH", "REBASE"),
		enumDef("RepositoryPermission", "ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"),
		enumDef("RepositoryVisibility", "PUBLIC", "PRIVATE", "INTERNAL"),
		enumDef("SearchType", "ISSUE", "ISSUE_ADVANCED", "REPOSITORY", "USER", "DISCUSSION"),
		enumDef("IssueOrderField", "CREATED_AT", "UPDATED_AT", "COMMENTS"),
		enumDef("IssueCommentOrderField", "UPDATED_AT"),
		enumDef("OrderDirection", "ASC", "DESC"),
		enumDef("PatchStatus", "ADDED", "DELETED", "RENAMED", "COPIED", "MODIFIED", "CHANGED"),
		enumDef("RepositoryAffiliation", "OWNER", "COLLABORATOR", "ORGANIZATION_MEMBER"),
		enumDef("RepositoryOrderField", "CREATED_AT", "UPDATED_AT", "PUSHED_AT", "NAME", "STARGAZERS"),
		enumDef("PullRequestUpdateState", "OPEN", "CLOSED"),
		inputDef("IssueOrder", arg("field", "IssueOrderField!"), arg("direction", "OrderDirection!")),
		inputDef("IssueCommentOrder", arg("field", "IssueCommentOrderField!"), arg("direction", "OrderDirection!")),
		inputDef("RepositoryOrder", arg("field", "RepositoryOrderField!"), arg("direction", "OrderDirection!")),
		inputDef("IssueFilters", arg("states", "[IssueState!]"), arg("labels", "[String!]"), arg("assignee", "String"), arg("createdBy", "String"), arg("mentioned", "String"), arg("since", "DateTime")),
		inputDef("CreateIssueInput", arg("repositoryId", "ID!"), arg("title", "String!"), arg("body", "String"), arg("labelIds", "[ID!]"), arg("assigneeIds", "[ID!]"), arg("milestoneId", "ID"), arg("projectIds", "[ID!]"), arg("issueTypeId", "ID"), arg("issueTemplate", "String"), arg("clientMutationId", "String")),
		inputDef("CloseIssueInput", arg("issueId", "ID!"), arg("stateReason", "IssueClosedStateReason"), arg("duplicateIssueId", "ID"), arg("clientMutationId", "String")),
		inputDef("ReopenIssueInput", arg("issueId", "ID!"), arg("clientMutationId", "String")),
		inputDef("UpdateIssueInput", arg("id", "ID!"), arg("title", "String"), arg("body", "String"), arg("state", "IssueState"), arg("labelIds", "[ID!]"), arg("assigneeIds", "[ID!]"), arg("milestoneId", "ID"), arg("projectIds", "[ID!]"), arg("clientMutationId", "String")),
		inputDef("AddCommentInput", arg("subjectId", "ID!"), arg("body", "String!"), arg("clientMutationId", "String")),
		inputDef("CreatePullRequestInput", arg("repositoryId", "ID!"), arg("baseRefName", "String!"), arg("headRefName", "String!"), arg("headRepositoryId", "ID"), arg("title", "String!"), arg("body", "String"), arg("draft", "Boolean"), arg("maintainerCanModify", "Boolean"), arg("clientMutationId", "String")),
		inputDef("MergePullRequestInput", arg("pullRequestId", "ID!"), arg("mergeMethod", "PullRequestMergeMethod"), arg("commitHeadline", "String"), arg("commitBody", "String"), arg("expectedHeadOid", "GitObjectID"), arg("authorEmail", "String"), arg("clientMutationId", "String")),
		inputDef("ClosePullRequestInput", arg("pullRequestId", "ID!"), arg("clientMutationId", "String")),
		inputDef("ReopenPullRequestInput", arg("pullRequestId", "ID!"), arg("clientMutationId", "String")),
		inputDef("UpdatePullRequestInput", arg("pullRequestId", "ID!"), arg("title", "String"), arg("body", "String"), arg("baseRefName", "String"), arg("state", "PullRequestUpdateState"), arg("labelIds", "[ID!]"), arg("assigneeIds", "[ID!]"), arg("clientMutationId", "String")),
		inputDef("CreateRepositoryInput", arg("name", "String!"), arg("ownerId", "ID"), arg("description", "String"), arg("visibility", "RepositoryVisibility!"), arg("template", "Boolean"), arg("homepageUrl", "URI"), arg("hasWikiEnabled", "Boolean"), arg("hasIssuesEnabled", "Boolean"), arg("teamId", "ID"), arg("clientMutationId", "String")),
		inputDef("AddLabelsToLabelableInput", arg("labelableId", "ID!"), arg("labelIds", "[ID!]!"), arg("clientMutationId", "String")),
	}

	query := objDef("Query", nil,
		f("viewer", "User!", constant(theUser)),
		f("user", "User", func(_ any, a map[string]any) any {
			if viewerLoginMatches(a) {
				return theUser
			}
			return nil
		}, arg("login", "String!")),
		f("repositoryOwner", "RepositoryOwner", func(_ any, a map[string]any) any {
			if viewerLoginMatches(a) {
				return theUser
			}
			return nil
		}, arg("login", "String!")),
		f("repository", "Repository", func(_ any, a map[string]any) any {
			owner, _ := a["owner"].(string)
			name, _ := a["name"].(string)
			if r := lookupRepo(owner, name); r != nil {
				return repoVal{r}
			}
			return nil
		}, arg("owner", "String!"), arg("name", "String!"), argDefault("followRenames", "Boolean", true)),
		f("node", "Node", func(_ any, a map[string]any) any { return nodeByID(a["id"]) }, arg("id", "ID!")),
		f("nodes", "[Node]!", func(_ any, a map[string]any) any {
			ids, _ := a["ids"].([]any)
			out := make([]any, len(ids))
			for i, id := range ids {
				out[i] = nodeByID(id)
			}
			return out
		}, arg("ids", "[ID!]!")),
		f("search", "SearchResultItemConnection!", func(_ any, a map[string]any) any {
			q, _ := a["query"].(string)
			t, _ := a["type"].(string)
			return search(q, t, a)
		}, withConnArgs(arg("query", "String!"), arg("type", "SearchType!"))...),
		f("rateLimit", "RateLimit", constant(Obj{{"limit", 5000}, {"cost", 1}, {"remaining", 4999}, {"used", 1}, {"resetAt", isoAt(3600)}, {"nodeCount", 0}}), argDefault("dryRun", "Boolean", false)),
	)

	mutation := objDef("Mutation", nil,
		f("createIssue", "CreateIssuePayload", func(_ any, a map[string]any) any {
			in := inputOf(a)
			r := requireRepoNode(in["repositoryId"])
			payloadIn := map[string]any{"title": in["title"], "body": in["body"]}
			if names, ok := labelNames(r, in["labelIds"]); ok {
				payloadIn["labels"] = names
			}
			i := createIssue(r, payloadIn)
			return Obj{{"clientMutationId", clientMutationID(in)}, {"issue", issueVal{r, i}}}
		}, arg("input", "CreateIssueInput!")),
		f("closeIssue", "CloseIssuePayload", func(_ any, a map[string]any) any {
			in := inputOf(a)
			v := requireIssueNode(in["issueId"])
			reason := "completed"
			if s, ok := in["stateReason"].(string); ok {
				reason = strings.ToLower(s)
			}
			setIssueState(v.issue, "closed", &reason)
			return Obj{{"clientMutationId", clientMutationID(in)}, {"issue", v}}
		}, arg("input", "CloseIssueInput!")),
		f("reopenIssue", "ReopenIssuePayload", func(_ any, a map[string]any) any {
			in := inputOf(a)
			v := requireIssueNode(in["issueId"])
			setIssueState(v.issue, "open", strPtr("reopened"))
			return Obj{{"clientMutationId", clientMutationID(in)}, {"issue", v}}
		}, arg("input", "ReopenIssueInput!")),
		f("updateIssue", "UpdateIssuePayload", func(_ any, a map[string]any) any {
			in := inputOf(a)
			v := requireIssueNode(in["id"])
			patch := map[string]any{}
			if t, ok := in["title"]; ok && t != nil {
				patch["title"] = t
			}
			if b, ok := in["body"]; ok {
				patch["body"] = b
			}
			if s, ok := in["state"].(string); ok {
				patch["state"] = strings.ToLower(s)
			}
			if names, ok := labelNames(v.repo, in["labelIds"]); ok {
				patch["labels"] = names
			}
			updateIssue(v.repo, v.issue, patch)
			return Obj{{"clientMutationId", clientMutationID(in)}, {"issue", v}}
		}, arg("input", "UpdateIssueInput!")),
		f("addComment", "AddCommentPayload", func(_ any, a map[string]any) any {
			in := inputOf(a)
			node := nodeByID(in["subjectId"])
			r, i := issueOf(node)
			if i == nil {
				panic(notFoundNode(in["subjectId"]))
			}
			c := addComment(i, in["body"])
			value := commentVal{r, i, c}
			edge := Obj{{"node", value}, {"cursor", cursorFor(len(i.Comments) - 1)}}
			return Obj{{"clientMutationId", clientMutationID(in)}, {"commentEdge", edge}, {"timelineEdge", edge}, {"subject", node}}
		}, arg("input", "AddCommentInput!")),
		f("addLabelsToLabelable", "AddLabelsToLabelablePayload", func(_ any, a map[string]any) any {
			in := inputOf(a)
			node := nodeByID(in["labelableId"])
			r, i := issueOf(node)
			if i == nil {
				panic(notFoundNode(in["labelableId"]))
			}
			names, _ := labelNames(r, in["labelIds"])
			for _, k := range labelKeys(r, names) {
				if !containsString(i.Labels, k) {
					i.Labels = append(i.Labels, k)
				}
			}
			return Obj{{"clientMutationId", clientMutationID(in)}, {"labelable", node}}
		}, arg("input", "AddLabelsToLabelableInput!")),
		f("createPullRequest", "CreatePullRequestPayload", func(_ any, a map[string]any) any {
			in := inputOf(a)
			r := requireRepoNode(in["repositoryId"])
			p := createPull(r, map[string]any{"title": in["title"], "body": in["body"], "head": in["headRefName"], "base": in["baseRefName"], "draft": in["draft"] == true})
			return Obj{{"clientMutationId", clientMutationID(in)}, {"pullRequest", pullVal{r, p}}}
		}, arg("input", "CreatePullRequestInput!")),
		f("mergePullRequest", "MergePullRequestPayload", func(_ any, a map[string]any) any {
			in := inputOf(a)
			v := requirePullNode(in["pullRequestId"])
			method := "merge"
			if m, ok := in["mergeMethod"].(string); ok {
				method = strings.ToLower(m)
			}
			title, _ := in["commitHeadline"].(string)
			body, _ := in["commitBody"].(string)
			sha, _ := in["expectedHeadOid"].(string)
			func() {
				defer func() {
					if r := recover(); r != nil {
						if he, ok := r.(*HTTPError); ok && he.Status == 405 {
							panic(fieldErr("Pull Request is not mergeable", "UNPROCESSABLE"))
						}
						panic(r)
					}
				}()
				mergePull(v.repo, v.pull, method, title, body, sha)
			}()
			return Obj{{"clientMutationId", clientMutationID(in)}, {"pullRequest", v}}
		}, arg("input", "MergePullRequestInput!")),
		f("closePullRequest", "ClosePullRequestPayload", func(_ any, a map[string]any) any {
			in := inputOf(a)
			v := requirePullNode(in["pullRequestId"])
			updatePull(v.repo, v.pull, map[string]any{"state": "closed"})
			return Obj{{"clientMutationId", clientMutationID(in)}, {"pullRequest", v}}
		}, arg("input", "ClosePullRequestInput!")),
		f("reopenPullRequest", "ReopenPullRequestPayload", func(_ any, a map[string]any) any {
			in := inputOf(a)
			v := requirePullNode(in["pullRequestId"])
			updatePull(v.repo, v.pull, map[string]any{"state": "open"})
			return Obj{{"clientMutationId", clientMutationID(in)}, {"pullRequest", v}}
		}, arg("input", "ReopenPullRequestInput!")),
		f("updatePullRequest", "UpdatePullRequestPayload", func(_ any, a map[string]any) any {
			in := inputOf(a)
			v := requirePullNode(in["pullRequestId"])
			patch := map[string]any{}
			if t, ok := in["title"].(string); ok {
				patch["title"] = t
			}
			if b, ok := in["body"]; ok {
				patch["body"] = b
			}
			if b, ok := in["baseRefName"].(string); ok {
				patch["base"] = b
			}
			if s, ok := in["state"].(string); ok {
				patch["state"] = strings.ToLower(s)
			}
			updatePull(v.repo, v.pull, patch)
			return Obj{{"clientMutationId", clientMutationID(in)}, {"pullRequest", v}}
		}, arg("input", "UpdatePullRequestInput!")),
		f("createRepository", "CreateRepositoryPayload", func(_ any, a map[string]any) any {
			in := inputOf(a)
			vis, _ := in["visibility"].(string)
			r := createRepository(map[string]any{"name": in["name"], "description": in["description"], "visibility": strings.ToLower(vis), "homepage": in["homepageUrl"]})
			return Obj{{"clientMutationId", clientMutationID(in)}, {"repository", repoVal{r}}}
		}, arg("input", "CreateRepositoryInput!")),
	)

	user := objDef("User", []string{"Node", "Actor", "RepositoryOwner"},
		f("id", "ID!", func(any, map[string]any) any { return nodeID("User", config.Viewer.ID) }),
		f("databaseId", "Int", func(any, map[string]any) any { return config.Viewer.ID }),
		f("login", "String!", func(any, map[string]any) any { return config.Viewer.Login }),
		f("name", "String", func(any, map[string]any) any { return config.Viewer.Name }),
		f("email", "String!", func(any, map[string]any) any { return config.Viewer.Email }),
		f("url", "URI!", func(any, map[string]any) any { return webURL(config.Viewer.Login) }),
		f("avatarUrl", "URI!", func(any, map[string]any) any { return webURL("avatars/" + config.Viewer.Login) }, arg("size", "Int")),
		f("company", "String", constant(nil)),
		f("bio", "String", constant(nil)),
		f("createdAt", "DateTime!", constant("2025-01-01T00:00:00Z")),
		f("repositories", "RepositoryConnection!", func(_ any, a map[string]any) any {
			items := []any{}
			for _, r := range store.List(config.Viewer.Login) {
				switch a["privacy"] {
				case "PUBLIC":
					if r.IsPrivate {
						continue
					}
				case "PRIVATE":
					if !r.IsPrivate {
						continue
					}
				}
				items = append(items, repoVal{r})
			}
			return connection(items, a)
		}, withConnArgs(arg("privacy", "RepositoryVisibility"), arg("ownerAffiliations", "[RepositoryAffiliation]"), arg("orderBy", "RepositoryOrder"), arg("isFork", "Boolean"))...),
		f("repository", "Repository", func(_ any, a map[string]any) any {
			name, _ := a["name"].(string)
			if r := lookupRepo(config.Viewer.Login, name); r != nil {
				return repoVal{r}
			}
			return nil
		}, arg("name", "String!")),
	)

	rv := func(v any) *Repo { return v.(repoVal).repo }
	repository := objDef("Repository", []string{"Node"},
		f("id", "ID!", func(v any, _ map[string]any) any { return nodeID("Repository", rv(v).ID) }),
		f("databaseId", "Int", func(v any, _ map[string]any) any { return rv(v).ID }),
		f("name", "String!", func(v any, _ map[string]any) any { return rv(v).Name }),
		f("nameWithOwner", "String!", func(v any, _ map[string]any) any { return rv(v).FullName() }),
		f("owner", "RepositoryOwner!", constant(theUser)),
		f("description", "String", func(v any, _ map[string]any) any { return nullable(rv(v).Description) }),
		f("homepageUrl", "URI", func(v any, _ map[string]any) any { return nullable(rv(v).Homepage) }),
		f("url", "URI!", func(v any, _ map[string]any) any { return webURL(rv(v).FullName()) }),
		f("sshUrl", "String!", func(v any, _ map[string]any) any { return "git@forge.local:" + rv(v).FullName() + ".git" }),
		f("isPrivate", "Boolean!", func(v any, _ map[string]any) any { return rv(v).IsPrivate }),
		f("visibility", "RepositoryVisibility!", func(v any, _ map[string]any) any {
			if rv(v).IsPrivate {
				return "PRIVATE"
			}
			return "PUBLIC"
		}),
		f("isFork", "Boolean!", constant(false)),
		f("isArchived", "Boolean!", constant(false)),
		f("isTemplate", "Boolean!", constant(false)),
		f("isEmpty", "Boolean!", func(v any, _ map[string]any) any { return rv(v).IsEmpty() }),
		f("isInOrganization", "Boolean!", constant(false)),
		f("hasIssuesEnabled", "Boolean!", constant(true)),
		f("hasWikiEnabled", "Boolean!", constant(false)),
		f("hasProjectsEnabled", "Boolean!", constant(true)),
		f("hasDiscussionsEnabled", "Boolean!", constant(false)),
		f("mergeCommitAllowed", "Boolean!", constant(true)),
		f("squashMergeAllowed", "Boolean!", constant(true)),
		f("rebaseMergeAllowed", "Boolean!", constant(true)),
		f("deleteBranchOnMerge", "Boolean!", constant(false)),
		f("stargazerCount", "Int!", constant(0)),
		f("forkCount", "Int!", constant(0)),
		f("viewerPermission", "RepositoryPermission", constant("ADMIN")),
		f("viewerCanAdminister", "Boolean!", constant(true)),
		f("createdAt", "DateTime!", func(v any, _ map[string]any) any { return rv(v).CreatedAt }),
		f("updatedAt", "DateTime!", func(v any, _ map[string]any) any { return rv(v).UpdatedAt }),
		f("pushedAt", "DateTime", func(v any, _ map[string]any) any { return nullable(rv(v).PushedAt) }),
		f("parent", "Repository", constant(nil)),
		f("defaultBranchRef", "Ref", func(v any, _ map[string]any) any {
			r := rv(v)
			if tip, ok := r.BranchTip(r.DefaultBranch); ok {
				return refVal{r, r.DefaultBranch, "refs/heads/", tip}
			}
			return nil
		}),
		f("ref", "Ref", func(v any, a map[string]any) any {
			r := rv(v)
			name, _ := a["qualifiedName"].(string)
			full := name
			if !strings.HasPrefix(full, "refs/") {
				full = "refs/heads/" + name
			}
			tip, ok := r.Refs[full]
			if !ok {
				return nil
			}
			slash := strings.IndexByte(full[5:], '/') + 5
			return refVal{r, full[slash+1:], full[:slash+1], tip}
		}, arg("qualifiedName", "String!")),
		f("refs", "RefConnection", func(v any, a map[string]any) any {
			r := rv(v)
			prefix, _ := a["refPrefix"].(string)
			q, hasQ := a["query"].(string)
			items := []any{}
			for _, ref := range r.SortedRefs() {
				if strings.HasPrefix(ref[0], prefix) && (!hasQ || strings.Contains(ref[0], q)) {
					items = append(items, refVal{r, ref[0][len(prefix):], prefix, ref[1]})
				}
			}
			return connection(items, a)
		}, withConnArgs(arg("refPrefix", "String!"), arg("query", "String"))...),
		f("issue", "Issue", func(v any, a map[string]any) any {
			r := rv(v)
			n, _ := numArg(a, "number")
			i := r.Issues[n]
			if i == nil {
				return nil
			}
			if i.IsPull {
				panic(fieldErr(fmt.Sprintf("Could not resolve to an Issue with the number of %d.", n), "NOT_FOUND"))
			}
			return issueVal{r, i}
		}, arg("number", "Int!")),
		f("issueOrPullRequest", "IssueOrPullRequest", func(v any, a map[string]any) any {
			r := rv(v)
			n, _ := numArg(a, "number")
			i := r.Issues[n]
			if i == nil {
				return nil
			}
			if p := r.Pulls[n]; i.IsPull && p != nil {
				return pullVal{r, p}
			}
			return issueVal{r, i}
		}, arg("number", "Int!")),
		f("issues", "IssueConnection!", func(v any, a map[string]any) any {
			r := rv(v)
			filter, _ := a["filterBy"].(map[string]any)
			states := stringList(a["states"])
			if a["states"] == nil && filter != nil {
				states = stringList(filter["states"])
			}
			labels := stringList(a["labels"])
			if a["labels"] == nil && filter != nil {
				labels = stringList(filter["labels"])
			}
			var list []*Issue
			for _, i := range r.SortedIssues() {
				if i.IsPull {
					continue
				}
				if len(states) > 0 && !containsString(states, strings.ToUpper(i.State)) {
					continue
				}
				ok := true
				for _, l := range labels {
					if !containsString(i.Labels, strings.ToLower(l)) {
						ok = false
					}
				}
				if ok {
					list = append(list, i)
				}
			}
			items := []any{}
			for _, i := range gqlSortIssues(list, a["orderBy"]) {
				items = append(items, issueVal{r, i})
			}
			return connection(items, a)
		}, withConnArgs(arg("states", "[IssueState!]"), arg("orderBy", "IssueOrder"), arg("labels", "[String!]"), arg("filterBy", "IssueFilters"))...),
		f("pullRequest", "PullRequest", func(v any, a map[string]any) any {
			r := rv(v)
			n, _ := numArg(a, "number")
			if p := r.Pulls[n]; p != nil {
				return pullVal{r, p}
			}
			return nil
		}, arg("number", "Int!")),
		f("pullRequests", "PullRequestConnection!", func(v any, a map[string]any) any {
			r := rv(v)
			states := stringList(a["states"])
			head, hasHead := a["headRefName"].(string)
			base, hasBase := a["baseRefName"].(string)
			var issues []*Issue
			for _, p := range r.SortedPulls() {
				if len(states) > 0 && !containsString(states, pullState(p)) {
					continue
				}
				if hasHead && p.HeadRef != head {
					continue
				}
				if hasBase && p.BaseRef != base {
					continue
				}
				issues = append(issues, p.Issue)
			}
			items := []any{}
			for _, i := range gqlSortIssues(issues, a["orderBy"]) {
				items = append(items, pullVal{r, r.Pulls[i.Number]})
			}
			return connection(items, a)
		}, withConnArgs(arg("states", "[PullRequestState!]"), arg("headRefName", "String"), arg("baseRefName", "String"), arg("labels", "[String!]"), arg("orderBy", "IssueOrder"))...),
		f("labels", "LabelConnection", func(v any, a map[string]any) any {
			r := rv(v)
			q, hasQ := a["query"].(string)
			items := []any{}
			for _, l := range r.SortedLabels() {
				if !hasQ || strings.Contains(strings.ToLower(l.Name), strings.ToLower(q)) {
					items = append(items, labelVal{r, l})
				}
			}
			return connection(items, a)
		}, withConnArgs(arg("query", "String"))...),
		f("label", "Label", func(v any, a map[string]any) any {
			r := rv(v)
			name, _ := a["name"].(string)
			if l := r.Labels[strings.ToLower(name)]; l != nil {
				return labelVal{r, l}
			}
			return nil
		}, arg("name", "String!")),
		f("assignableUsers", "UserConnection!", func(_ any, a map[string]any) any { return connection([]any{theUser}, a) }, withConnArgs(arg("query", "String"))...),
		f("mentionableUsers", "UserConnection!", func(_ any, a map[string]any) any { return connection([]any{theUser}, a) }, withConnArgs(arg("query", "String"))...),
		f("milestones", "MilestoneConnection", func(_ any, a map[string]any) any { return connection([]any{}, a) }, connectionArgs...),
	)

	ref := objDef("Ref", []string{"Node"},
		f("id", "ID!", func(v any, _ map[string]any) any {
			x := v.(refVal)
			return nodeID("Ref", fmt.Sprintf("%d:%s%s", x.repo.ID, x.prefix, x.name))
		}),
		f("name", "String!", func(v any, _ map[string]any) any { return v.(refVal).name }),
		f("prefix", "String!", func(v any, _ map[string]any) any { return v.(refVal).prefix }),
		f("repository", "Repository!", func(v any, _ map[string]any) any { return repoVal{v.(refVal).repo} }),
		f("target", "GitObject", func(v any, _ map[string]any) any {
			x := v.(refVal)
			typ := "Commit"
			if obj := x.repo.Get(x.target); obj != nil && obj.Type == "tag" {
				typ = "Tag"
			}
			return commitVal{x.repo, x.target, typ}
		}),
	)

	cv := func(v any) commitVal { return v.(commitVal) }
	commit := objDef("Commit", []string{"Node", "GitObject"},
		f("id", "ID!", func(v any, _ map[string]any) any { return nodeID("Commit", cv(v).oid) }),
		f("oid", "GitObjectID!", func(v any, _ map[string]any) any { return cv(v).oid }),
		f("abbreviatedOid", "String!", func(v any, _ map[string]any) any { return cv(v).oid[:7] }),
		f("message", "String!", func(v any, _ map[string]any) any {
			if c := cv(v).repo.Commit(cv(v).oid); c != nil {
				return strings.TrimSuffix(c.Message, "\n")
			}
			return ""
		}),
		f("messageHeadline", "String!", func(v any, _ map[string]any) any {
			if c := cv(v).repo.Commit(cv(v).oid); c != nil {
				return strings.SplitN(c.Message, "\n", 2)[0]
			}
			return ""
		}),
		f("committedDate", "DateTime!", func(any, map[string]any) any { return isoAt(0) }),
		f("url", "URI!", func(v any, _ map[string]any) any { return webURL(cv(v).repo.FullName() + "/commit/" + cv(v).oid) }),
	)
	tag := objDef("Tag", []string{"Node", "GitObject"},
		f("id", "ID!", func(v any, _ map[string]any) any { return nodeID("Commit", cv(v).oid) }),
		f("oid", "GitObjectID!", func(v any, _ map[string]any) any { return cv(v).oid }),
		f("abbreviatedOid", "String!", func(v any, _ map[string]any) any { return cv(v).oid[:7] }),
	)

	iv := func(v any) issueVal { return v.(issueVal) }
	issue := objDef("Issue", []string{"Node", "Labelable", "Comment"}, flatten([]*FieldDef{
		f("id", "ID!", func(v any, _ map[string]any) any { return nodeID("Issue", iv(v).issue.ID) }),
		f("databaseId", "Int", func(v any, _ map[string]any) any { return iv(v).issue.ID }),
		f("url", "URI!", func(v any, _ map[string]any) any { return webURL(fmt.Sprintf("%s/issues/%d", iv(v).repo.FullName(), iv(v).issue.Number)) }),
		f("state", "IssueState!", func(v any, _ map[string]any) any { return strings.ToUpper(iv(v).issue.State) }),
		f("stateReason", "IssueStateReason", func(v any, _ map[string]any) any {
			if sr := iv(v).issue.StateReason; sr != nil {
				return strings.ToUpper(*sr)
			}
			return nil
		}),
	}, commonIssueFields(func(v any) *Issue { return v.(issueVal).issue }))...)

	pv := func(v any) pullVal { return v.(pullVal) }
	pull := objDef("PullRequest", []string{"Node", "Labelable", "Comment"}, flatten([]*FieldDef{
		f("id", "ID!", func(v any, _ map[string]any) any { return nodeID("PullRequest", pv(v).pull.ID) }),
		f("databaseId", "Int", func(v any, _ map[string]any) any { return pv(v).pull.ID }),
		f("url", "URI!", func(v any, _ map[string]any) any { return webURL(fmt.Sprintf("%s/pull/%d", pv(v).repo.FullName(), pv(v).pull.Number)) }),
		f("state", "PullRequestState!", func(v any, _ map[string]any) any { return pullState(pv(v).pull) }),
		f("merged", "Boolean!", func(v any, _ map[string]any) any { return pv(v).pull.Merged }),
		f("mergedAt", "DateTime", func(v any, _ map[string]any) any { return nullable(pv(v).pull.MergedAt) }),
		f("mergedBy", "Actor", func(v any, _ map[string]any) any {
			if pv(v).pull.Merged {
				return theUser
			}
			return nil
		}),
		f("mergeCommit", "Commit", func(v any, _ map[string]any) any {
			if sha := pv(v).pull.MergeCommitSha; sha != nil {
				return commitVal{pv(v).repo, *sha, "Commit"}
			}
			return nil
		}),
		f("isDraft", "Boolean!", func(v any, _ map[string]any) any { return pv(v).pull.Draft }),
		f("headRefName", "String!", func(v any, _ map[string]any) any { return pv(v).pull.HeadRef }),
		f("baseRefName", "String!", func(v any, _ map[string]any) any { return pv(v).pull.BaseRef }),
		f("headRefOid", "GitObjectID!", func(v any, _ map[string]any) any { return pullHeadSha(pv(v).repo, pv(v).pull) }),
		f("baseRefOid", "GitObjectID!", func(v any, _ map[string]any) any { return pullBaseSha(pv(v).repo, pv(v).pull) }),
		f("headRepository", "Repository", func(v any, _ map[string]any) any { return repoVal{pv(v).repo} }),
		f("baseRepository", "Repository", func(v any, _ map[string]any) any { return repoVal{pv(v).repo} }),
		f("headRepositoryOwner", "RepositoryOwner", constant(theUser)),
		f("isCrossRepository", "Boolean!", constant(false)),
		f("maintainerCanModify", "Boolean!", constant(false)),
		f("mergeable", "MergeableState!", func(v any, _ map[string]any) any {
			switch pullMergeable(pv(v).repo, pv(v).pull) {
			case 1:
				return "MERGEABLE"
			case 0:
				return "CONFLICTING"
			}
			return "UNKNOWN"
		}),
		f("additions", "Int!", func(v any, _ map[string]any) any {
			n := 0
			for _, fl := range pullFiles(pv(v).repo, pv(v).pull) {
				n += fl.Additions
			}
			return n
		}),
		f("deletions", "Int!", func(v any, _ map[string]any) any {
			n := 0
			for _, fl := range pullFiles(pv(v).repo, pv(v).pull) {
				n += fl.Deletions
			}
			return n
		}),
		f("changedFiles", "Int!", func(v any, _ map[string]any) any { return len(pullFiles(pv(v).repo, pv(v).pull)) }),
		f("files", "PullRequestChangedFileConnection", func(v any, a map[string]any) any {
			items := []any{}
			for _, fl := range pullFiles(pv(v).repo, pv(v).pull) {
				change := "MODIFIED"
				switch fl.Status {
				case "added":
					change = "ADDED"
				case "removed":
					change = "DELETED"
				case "renamed":
					change = "RENAMED"
				}
				items = append(items, Obj{{"path", fl.Filename}, {"additions", fl.Additions}, {"deletions", fl.Deletions}, {"changeType", change}})
			}
			return connection(items, a)
		}, connectionArgs...),
		f("commits", "PullRequestCommitConnection!", func(v any, a map[string]any) any {
			items := []any{}
			for _, oid := range pullCommits(pv(v).repo, pv(v).pull) {
				items = append(items, Obj{{"commit", commitVal{pv(v).repo, oid, "Commit"}}})
			}
			return connection(items, a)
		}, connectionArgs...),
	}, commonIssueFields(func(v any) *Issue { return v.(pullVal).pull.Issue }))...)

	ccv := func(v any) commentVal { return v.(commentVal) }
	comment := objDef("IssueComment", []string{"Node", "Comment"},
		f("id", "ID!", func(v any, _ map[string]any) any { return nodeID("IssueComment", ccv(v).comment.ID) }),
		f("databaseId", "Int", func(v any, _ map[string]any) any { return ccv(v).comment.ID }),
		f("body", "String!", func(v any, _ map[string]any) any { return ccv(v).comment.Body }),
		f("bodyText", "String!", func(v any, _ map[string]any) any { return ccv(v).comment.Body }),
		f("author", "Actor", constant(theUser)),
		f("createdAt", "DateTime!", func(v any, _ map[string]any) any { return ccv(v).comment.CreatedAt }),
		f("updatedAt", "DateTime!", func(v any, _ map[string]any) any { return ccv(v).comment.UpdatedAt }),
		f("url", "URI!", func(v any, _ map[string]any) any {
			x := ccv(v)
			return webURL(fmt.Sprintf("%s/issues/%d#issuecomment-%d", x.repo.FullName(), x.issue.Number, x.comment.ID))
		}),
		f("issue", "Issue!", func(v any, _ map[string]any) any { return issueVal{ccv(v).repo, ccv(v).issue} }),
	)

	lv := func(v any) labelVal { return v.(labelVal) }
	label := objDef("Label", []string{"Node"},
		f("id", "ID!", func(v any, _ map[string]any) any { return nodeID("Label", lv(v).label.ID) }),
		f("name", "String!", func(v any, _ map[string]any) any { return lv(v).label.Name }),
		f("color", "String!", func(v any, _ map[string]any) any { return lv(v).label.Color }),
		f("description", "String", func(v any, _ map[string]any) any { return nullable(lv(v).label.Description) }),
		f("isDefault", "Boolean!", constant(false)),
	)

	types = append(types, query, mutation,
		payload("CreateIssuePayload", [2]string{"issue", "Issue"}),
		payload("CloseIssuePayload", [2]string{"issue", "Issue"}),
		payload("ReopenIssuePayload", [2]string{"issue", "Issue"}),
		payload("UpdateIssuePayload", [2]string{"issue", "Issue"}),
		payload("AddCommentPayload", [2]string{"commentEdge", "IssueCommentEdge"}, [2]string{"timelineEdge", "IssueTimelineItemEdge"}, [2]string{"subject", "Node"}),
		payload("AddLabelsToLabelablePayload", [2]string{"labelable", "Labelable"}),
		payload("CreatePullRequestPayload", [2]string{"pullRequest", "PullRequest"}),
		payload("MergePullRequestPayload", [2]string{"pullRequest", "PullRequest"}),
		payload("ClosePullRequestPayload", [2]string{"pullRequest", "PullRequest"}),
		payload("ReopenPullRequestPayload", [2]string{"pullRequest", "PullRequest"}),
		payload("UpdatePullRequestPayload", [2]string{"pullRequest", "PullRequest"}),
		payload("CreateRepositoryPayload", [2]string{"repository", "Repository"}),
		objDef("IssueTimelineItemEdge", nil, f("node", "IssueComment", nil), f("cursor", "String!", nil)),
		user, repository, ref, commit, tag, issue, pull,
		objDef("PullRequestChangedFile", nil, f("path", "String!", nil), f("additions", "Int!", nil), f("deletions", "Int!", nil), f("changeType", "PatchStatus!", nil)),
		objDef("PullRequestCommit", nil, f("commit", "Commit!", nil)),
		comment, label,
		objDef("Milestone", nil, f("id", "ID!", nil), f("title", "String!", nil), f("number", "Int!", nil)),
		objDef("PageInfo", nil, f("hasNextPage", "Boolean!", nil), f("hasPreviousPage", "Boolean!", nil), f("startCursor", "String", nil), f("endCursor", "String", nil)),
		objDef("RateLimit", nil, f("limit", "Int!", nil), f("cost", "Int!", nil), f("remaining", "Int!", nil), f("used", "Int!", nil), f("resetAt", "DateTime!", nil), f("nodeCount", "Int!", nil)),
		objDef("SearchResultItemConnection", nil, f("issueCount", "Int!", nil), f("repositoryCount", "Int!", nil), f("userCount", "Int!", nil), f("discussionCount", "Int!", nil), f("nodes", "[SearchResultItem]", nil), f("edges", "[SearchResultItemEdge]", nil), f("pageInfo", "PageInfo!", nil)),
		objDef("SearchResultItemEdge", nil, f("node", "SearchResultItem", nil), f("cursor", "String!", nil)),
	)
	for _, pair := range [][2]string{{"Issue", "Issue"}, {"PullRequest", "PullRequest"}, {"IssueComment", "IssueComment"}, {"Label", "Label"}, {"User", "User"}, {"Repository", "Repository"}, {"Ref", "Ref"}, {"Milestone", "Milestone"}, {"PullRequestChangedFile", "PullRequestChangedFile"}, {"PullRequestCommit", "PullRequestCommit"}} {
		types = append(types, connectionTypes(pair[0], pair[1])...)
	}
	sizeArg := arg("size", "Int")
	types = append(types,
		&TypeDef{Kind: "INTERFACE", Name: "Node", Fields: []*FieldDef{f("id", "ID!", nil)}, PossibleTypes: []string{"Repository", "Issue", "PullRequest", "IssueComment", "User", "Label", "Ref", "Commit", "Tag"}},
		&TypeDef{Kind: "INTERFACE", Name: "Actor", Fields: []*FieldDef{f("login", "String!", nil), f("url", "URI!", nil), f("avatarUrl", "URI!", nil, sizeArg)}, PossibleTypes: []string{"User"}},
		&TypeDef{Kind: "INTERFACE", Name: "RepositoryOwner", Fields: []*FieldDef{f("id", "ID!", nil), f("login", "String!", nil), f("url", "URI!", nil), f("avatarUrl", "URI!", nil, sizeArg)}, PossibleTypes: []string{"User"}},
		&TypeDef{Kind: "INTERFACE", Name: "GitObject", Fields: []*FieldDef{f("id", "ID!", nil), f("oid", "GitObjectID!", nil), f("abbreviatedOid", "String!", nil)}, PossibleTypes: []string{"Commit", "Tag"}},
		&TypeDef{Kind: "INTERFACE", Name: "Labelable", Fields: []*FieldDef{f("labels", "LabelConnection", nil, connectionArgs...)}, PossibleTypes: []string{"Issue", "PullRequest"}},
		&TypeDef{Kind: "INTERFACE", Name: "Comment", Fields: []*FieldDef{f("id", "ID!", nil), f("body", "String!", nil), f("author", "Actor", nil), f("createdAt", "DateTime!", nil)}, PossibleTypes: []string{"Issue", "PullRequest", "IssueComment"}},
		&TypeDef{Kind: "UNION", Name: "SearchResultItem", PossibleTypes: []string{"Issue", "PullRequest", "Repository", "User"}},
		&TypeDef{Kind: "UNION", Name: "IssueOrPullRequest", PossibleTypes: []string{"Issue", "PullRequest"}},
	)
	return buildSchema(types, "Query", "Mutation")
}

// ---------------------------------------------------------------- search

func search(query, typ string, args map[string]any) Obj {
	qualifiers := map[string][]string{}
	var words []string
	for _, token := range strings.Fields(query) {
		if colon := strings.IndexByte(token, ':'); colon > 0 {
			key := strings.ToLower(token[:colon])
			qualifiers[key] = append(qualifiers[key], strings.Trim(token[colon+1:], `"`))
		} else {
			words = append(words, strings.ToLower(token))
		}
	}
	matches := func(text string) bool {
		lower := strings.ToLower(text)
		for _, w := range words {
			if !strings.Contains(lower, w) {
				return false
			}
		}
		return true
	}
	repos := store.List(config.Viewer.Login)
	if filter, ok := qualifiers["repo"]; ok {
		var kept []*Repo
		for _, r := range repos {
			for _, f := range filter {
				if strings.EqualFold(f, r.FullName()) {
					kept = append(kept, r)
					break
				}
			}
		}
		repos = kept
	}
	items := []any{}
	issueCount, repositoryCount, userCount := 0, 0, 0
	switch typ {
	case "REPOSITORY":
		for _, r := range repos {
			desc := ""
			if r.Description != nil {
				desc = *r.Description
			}
			if matches(r.Name + " " + desc) {
				items = append(items, repoVal{r})
			}
		}
		repositoryCount = len(items)
	case "USER":
		if matches(config.Viewer.Login) {
			items = append(items, theUser)
		}
		userCount = len(items)
	case "ISSUE", "ISSUE_ADVANCED":
		is := qualifiers["is"]
		state := qualifiers["state"]
		wantPulls := containsString(is, "pr") || containsString(qualifiers["type"], "pr")
		wantIssues := containsString(is, "issue") || containsString(qualifiers["type"], "issue")
		open := containsString(is, "open") || containsString(state, "open")
		closed := containsString(is, "closed") || containsString(state, "closed") || containsString(is, "merged")
		labels := qualifiers["label"]
		type found struct {
			seq   int64
			value any
		}
		var all []found
		for _, r := range repos {
			for _, i := range r.SortedIssues() {
				if (wantPulls && !i.IsPull) || (wantIssues && i.IsPull) {
					continue
				}
				if open && !closed && i.State != "open" {
					continue
				}
				if closed && !open && i.State != "closed" {
					continue
				}
				ok := true
				for _, l := range labels {
					if !containsString(i.Labels, strings.ToLower(l)) {
						ok = false
					}
				}
				body := ""
				if i.Body != nil {
					body = *i.Body
				}
				if !ok || !matches(i.Title+" "+body) {
					continue
				}
				var value any = issueVal{r, i}
				if p := r.Pulls[i.Number]; i.IsPull && p != nil {
					value = pullVal{r, p}
				}
				all = append(all, found{i.Seq, value})
			}
		}
		sort.SliceStable(all, func(a, b int) bool { return all[a].seq > all[b].seq })
		for _, x := range all {
			items = append(items, x.value)
		}
		issueCount = len(items)
	}
	conn := connection(items, args)
	return append(conn, KV{"issueCount", issueCount}, KV{"repositoryCount", repositoryCount}, KV{"userCount", userCount}, KV{"discussionCount", 0})
}

var githubSchema *Schema

func handleGraphQL(r *Request) *Response {
	if !isAuthorized(r) {
		return errorResp(401, "This endpoint requires you to be authenticated.", nil)
	}
	if r.Method != "POST" {
		return errorResp(404, "Not Found", nil)
	}
	body, err := parseJSON(r.Body)
	if err != nil {
		return errorResp(400, "Problems parsing JSON", nil)
	}
	return jsonResp(200, executeGraphQL(githubSchema, body))
}
