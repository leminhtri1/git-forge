package main

// POST /mcp: the Model Context Protocol over Streamable HTTP, JSON-RPC 2.0 (the Go twin of src/mcp/).
// Both eras: revision 2026-07-28 (server/discover, no handshake) and the initialize handshake. No session
// is kept; every request carries the bearer token. The tools act on the same store as REST and answer
// with the same objects REST answers.

import (
	"encoding/base64"
	"fmt"
	"strings"
)

var mcpSupported = []any{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

const mcpHandshakeDefault = "2025-11-25"
const mcpInstructions = "Tools for a GitHub-compatible forge: repositories, issues and pull requests of the authenticated user."

var mcpServerInfo = Obj{{"name", "git-forge"}, {"title", "git-forge MCP server"}, {"version", "1.0.0"}}
var mcpCapabilities = Obj{{"tools", Obj{{"listChanged", false}}}}

type invalidArguments struct{ msg string }

type mcpTool struct {
	name, title, description string
	schema                   Obj
	readOnly                 bool
	run                      func(args map[string]any, u Urls) any
}

func propStr(desc string) Obj  { return Obj{{"type", "string"}, {"description", desc}} }
func propNum(desc string) Obj  { return Obj{{"type", "number"}, {"description", desc}} }
func propBool(desc string) Obj { return Obj{{"type", "boolean"}, {"description", desc}} }
func propEnum(desc string, values ...string) Obj {
	list := []any{}
	for _, v := range values {
		list = append(list, v)
	}
	return Obj{{"type", "string"}, {"enum", list}, {"description", desc}}
}
func propStrings(desc string) Obj {
	return Obj{{"type", "array"}, {"items", Obj{{"type", "string"}}}, {"description", desc}}
}

var ownerProp = propStr("Repository owner (the user login)")
var repoProp = propStr("Repository name")

func toolSchema(props Obj, required ...string) Obj {
	req := []any{}
	for _, r := range required {
		req = append(req, r)
	}
	return Obj{{"type", "object"}, {"properties", props}, {"required", req}, {"additionalProperties", false}}
}

func argText(args map[string]any, key string, required bool) string {
	v, present := args[key]
	if !present || v == nil || v == "" {
		if required {
			panic(&invalidArguments{"missing required argument: " + key})
		}
		return ""
	}
	s, ok := v.(string)
	if !ok {
		panic(&invalidArguments{"argument " + key + " must be a string"})
	}
	return s
}

func argInt(args map[string]any, key string, required bool) (int, bool) {
	v, present := args[key]
	if !present || v == nil || v == "" {
		if required {
			panic(&invalidArguments{"missing required argument: " + key})
		}
		return 0, false
	}
	n, ok := intOf(v)
	if !ok {
		panic(&invalidArguments{"argument " + key + " must be an integer"})
	}
	return n, true
}

func repoArgs(args map[string]any) *Repo {
	return requireRepo(argText(args, "owner", true), argText(args, "repo", true))
}

func pageSlice(n int, args map[string]any) (int, int) {
	per, ok := argInt(args, "perPage", false)
	if !ok || per < 1 {
		per = 30
	}
	if per > 100 {
		per = 100
	}
	page, ok := argInt(args, "page", false)
	if !ok || page < 1 {
		page = 1
	}
	start, end := (page-1)*per, page*per
	if start > n {
		start = n
	}
	if end > n {
		end = n
	}
	return start, end
}

var mcpTools = []*mcpTool{
	{name: "get_me", title: "Get my user profile", description: "Get the authenticated user: login, id and profile.", schema: toolSchema(Obj{}), readOnly: true,
		run: func(_ map[string]any, u Urls) any { return fullUserView(u) }},
	{name: "create_repo", title: "Create repository", description: "Create a new repository owned by the authenticated user.",
		schema: toolSchema(Obj{{"name", propStr("Repository name")}, {"description", propStr("Repository description")}, {"private", propBool("Whether the repository is private")}, {"autoInit", propBool("Initialize with a README")}}, "name"),
		run: func(a map[string]any, u Urls) any {
			return repoView(createRepository(map[string]any{"name": argText(a, "name", true), "description": a["description"], "private": a["private"], "auto_init": a["autoInit"]}), u)
		}},
	{name: "get_repo", title: "Get repository", description: "Get a repository by owner and name: its id, full name, visibility and default branch.",
		schema: toolSchema(Obj{{"owner", ownerProp}, {"repo", repoProp}}, "owner", "repo"), readOnly: true,
		run: func(a map[string]any, u Urls) any { return repoView(repoArgs(a), u) }},
	{name: "list_repos", title: "List repositories", description: "List the authenticated user's repositories, oldest first.",
		schema: toolSchema(Obj{{"page", propNum("Page number (1-based)")}, {"perPage", propNum("Results per page (max 100)")}}), readOnly: true,
		run: func(a map[string]any, u Urls) any {
			repos := store.List(config.Viewer.Login)
			start, end := pageSlice(len(repos), a)
			out := []any{}
			for _, r := range repos[start:end] {
				out = append(out, repoView(r, u))
			}
			return Obj{{"repositories", out}}
		}},
	{name: "list_branches", title: "List branches", description: "List the branches of a repository with their tip commits.",
		schema: toolSchema(Obj{{"owner", ownerProp}, {"repo", repoProp}}, "owner", "repo"), readOnly: true,
		run: func(a map[string]any, _ Urls) any {
			r := repoArgs(a)
			out := []any{}
			for _, ref := range r.SortedRefs() {
				if strings.HasPrefix(ref[0], "refs/heads/") {
					out = append(out, Obj{{"name", ref[0][11:]}, {"commit", Obj{{"sha", ref[1]}}}})
				}
			}
			return Obj{{"branches", out}}
		}},
	{name: "create_branch", title: "Create branch", description: "Create a branch from another branch (the default branch when from_branch is omitted).",
		schema: toolSchema(Obj{{"owner", ownerProp}, {"repo", repoProp}, {"branch", propStr("Name of the new branch")}, {"from_branch", propStr("Source branch (defaults to the default branch)")}}, "owner", "repo", "branch"),
		run: func(a map[string]any, _ Urls) any {
			r := repoArgs(a)
			from := argText(a, "from_branch", false)
			if from == "" {
				from = r.DefaultBranch
			}
			sha, ok := r.BranchTip(from)
			if !ok {
				fail(404, "Branch "+from+" not found")
			}
			ref := createRef(r, "refs/heads/"+argText(a, "branch", true), sha)
			return Obj{{"ref", ref}, {"object", Obj{{"sha", sha}, {"type", "commit"}}}}
		}},
	{name: "get_file_contents", title: "Get file or directory contents", description: "Get the contents of a file (base64) or a directory listing at a path and optional ref.",
		schema: toolSchema(Obj{{"owner", ownerProp}, {"repo", repoProp}, {"path", propStr("Path to the file or directory")}, {"ref", propStr("Branch, tag or commit (defaults to the default branch)")}}, "owner", "repo", "path"), readOnly: true,
		run: func(a map[string]any, u Urls) any {
			r := repoArgs(a)
			ref := argText(a, "ref", false)
			if ref == "" {
				ref = r.DefaultBranch
			}
			tip, ok := r.Resolve(ref)
			if !ok {
				fail(404, "No commit found for the ref "+ref)
			}
			var parts []string
			for _, p := range strings.Split(argText(a, "path", false), "/") {
				if p != "" {
					parts = append(parts, p)
				}
			}
			path := strings.Join(parts, "/")
			entry, ok := r.EntryAt(r.Commit(tip).Tree, path)
			if !ok {
				fail(404, "Not Found")
			}
			if isTreeMode(entry.Mode) {
				out := []any{}
				for _, c := range r.Tree(entry.ID) {
					child := c.Name
					if path != "" {
						child = path + "/" + c.Name
					}
					size := 0
					if !isTreeMode(c.Mode) {
						if obj := r.Get(c.ID); obj != nil {
							size = len(obj.Data)
						}
					}
					out = append(out, contentEntryView(r, child, c.Mode, c.ID, size, ref, u))
				}
				return Obj{{"entries", out}}
			}
			blob := r.Get(entry.ID)
			return append(contentEntryView(r, path, entry.Mode, entry.ID, len(blob.Data), ref, u), KV{"encoding", "base64"}, KV{"content", base64.StdEncoding.EncodeToString(blob.Data)})
		}},
	{name: "create_or_update_file", title: "Create or update file", description: "Create or update a single file with a commit. Updating an existing file requires its current blob sha.",
		schema: toolSchema(Obj{{"owner", ownerProp}, {"repo", repoProp}, {"path", propStr("Path of the file")}, {"content", propStr("New file content (plain text)")}, {"message", propStr("Commit message")}, {"branch", propStr("Branch to commit to")}, {"sha", propStr("Blob sha of the file being replaced")}}, "owner", "repo", "path", "content", "message"),
		run: func(a map[string]any, u Urls) any {
			r := repoArgs(a)
			in := map[string]any{"content": base64.StdEncoding.EncodeToString([]byte(argText(a, "content", true))), "message": argText(a, "message", true)}
			if b := argText(a, "branch", false); b != "" {
				in["branch"] = b
			}
			if s := argText(a, "sha", false); s != "" {
				in["sha"] = s
			}
			res := putFile(r, argText(a, "path", true), in)
			return Obj{{"content", contentEntryView(r, res.Path, "100644", res.BlobID, res.Size, res.Branch, u)}, {"commit", gitCommitView(r, res.CommitID, u)}}
		}},
	{name: "create_issue", title: "Open new issue", description: "Create an issue in a repository. Labels must already exist in the repository.",
		schema: toolSchema(Obj{{"owner", ownerProp}, {"repo", repoProp}, {"title", propStr("Issue title")}, {"body", propStr("Issue body (Markdown)")}, {"labels", propStrings("Label names to apply")}}, "owner", "repo", "title"),
		run: func(a map[string]any, u Urls) any {
			r := repoArgs(a)
			in := map[string]any{"title": argText(a, "title", true), "labels": a["labels"]}
			if b := argText(a, "body", false); b != "" {
				in["body"] = b
			}
			return issueView(r, createIssue(r, in), u)
		}},
	{name: "get_issue", title: "Get issue details", description: "Get one issue of a repository by its number.",
		schema: toolSchema(Obj{{"owner", ownerProp}, {"repo", repoProp}, {"issue_number", propNum("Issue number")}}, "owner", "repo", "issue_number"), readOnly: true,
		run: func(a map[string]any, u Urls) any {
			r := repoArgs(a)
			n, _ := argInt(a, "issue_number", true)
			return issueView(r, requireIssue(r, n), u)
		}},
	{name: "list_issues", title: "List issues", description: "List the issues of a repository, filtered by state (open, closed or all; default open), newest first.",
		schema: toolSchema(Obj{{"owner", ownerProp}, {"repo", repoProp}, {"state", propEnum("Filter by state", "open", "closed", "all")}, {"labels", propStrings("Only issues carrying all of these labels")}, {"direction", propEnum("Sort direction by creation", "asc", "desc")}, {"page", propNum("Page number (1-based)")}, {"perPage", propNum("Results per page (max 100)")}}, "owner", "repo"), readOnly: true,
		run: func(a map[string]any, u Urls) any {
			r := repoArgs(a)
			state := strings.ToLower(argText(a, "state", false))
			if state == "" {
				state = "open"
			}
			var labels []string
			if list, ok := a["labels"].([]any); ok {
				for _, l := range list {
					labels = append(labels, strings.ToLower(fmt.Sprint(l)))
				}
			}
			var list []*Issue
			for _, i := range r.SortedIssues() {
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
			dir := argText(a, "direction", false)
			list = sortIssues(list, "created", map[bool]string{true: "asc", false: "desc"}[dir == "asc"])
			start, end := pageSlice(len(list), a)
			issues := []any{}
			for _, i := range list[start:end] {
				issues = append(issues, issueView(r, i, u))
			}
			return Obj{{"issues", issues}, {"totalCount", len(list)}}
		}},
	{name: "update_issue", title: "Edit issue", description: "Update an issue: title, body, state (open or closed) or labels.",
		schema: toolSchema(Obj{{"owner", ownerProp}, {"repo", repoProp}, {"issue_number", propNum("Issue number")}, {"title", propStr("New title")}, {"body", propStr("New body")}, {"state", propEnum("New state", "open", "closed")}, {"labels", propStrings("Replace the labels")}}, "owner", "repo", "issue_number"),
		run: func(a map[string]any, u Urls) any {
			r := repoArgs(a)
			n, _ := argInt(a, "issue_number", true)
			i := requireIssue(r, n)
			patch := map[string]any{}
			for _, k := range []string{"title", "body", "state", "labels"} {
				if v, ok := a[k]; ok {
					patch[k] = v
				}
			}
			return issueView(r, updateIssue(r, i, patch), u)
		}},
	{name: "add_issue_comment", title: "Add comment to issue", description: "Add a comment to an issue or pull request.",
		schema: toolSchema(Obj{{"owner", ownerProp}, {"repo", repoProp}, {"issue_number", propNum("Issue or pull request number")}, {"body", propStr("Comment text")}}, "owner", "repo", "issue_number", "body"),
		run: func(a map[string]any, u Urls) any {
			r := repoArgs(a)
			n, _ := argInt(a, "issue_number", true)
			i := requireIssue(r, n)
			return commentView(r, i, addComment(i, argText(a, "body", true)), u)
		}},
	{name: "list_labels", title: "List labels", description: "List the labels defined in a repository.",
		schema: toolSchema(Obj{{"owner", ownerProp}, {"repo", repoProp}}, "owner", "repo"), readOnly: true,
		run: func(a map[string]any, u Urls) any {
			r := repoArgs(a)
			out := []any{}
			for _, l := range r.SortedLabels() {
				out = append(out, labelView(r, l, u))
			}
			return Obj{{"labels", out}}
		}},
	{name: "create_pull_request", title: "Open new pull request", description: "Open a pull request from a head branch into a base branch.",
		schema: toolSchema(Obj{{"owner", ownerProp}, {"repo", repoProp}, {"title", propStr("Pull request title")}, {"head", propStr("Branch with the changes")}, {"base", propStr("Branch to merge into")}, {"body", propStr("Pull request description")}, {"draft", propBool("Open as a draft")}}, "owner", "repo", "title", "head", "base"),
		run: func(a map[string]any, u Urls) any {
			r := repoArgs(a)
			in := map[string]any{"title": argText(a, "title", true), "head": argText(a, "head", true), "base": argText(a, "base", true), "draft": a["draft"] == true}
			if b := argText(a, "body", false); b != "" {
				in["body"] = b
			}
			return pullView(r, createPull(r, in), u, true)
		}},
	{name: "get_pull_request", title: "Get pull request details", description: "Get one pull request by its number, with its head, base and merge state.",
		schema: toolSchema(Obj{{"owner", ownerProp}, {"repo", repoProp}, {"pull_number", propNum("Pull request number")}}, "owner", "repo", "pull_number"), readOnly: true,
		run: func(a map[string]any, u Urls) any {
			r := repoArgs(a)
			n, _ := argInt(a, "pull_number", true)
			return pullView(r, requirePull(r, n), u, true)
		}},
	{name: "list_pull_requests", title: "List pull requests", description: "List the pull requests of a repository, filtered by state (open, closed or all; default open).",
		schema: toolSchema(Obj{{"owner", ownerProp}, {"repo", repoProp}, {"state", propEnum("Filter by state", "open", "closed", "all")}, {"page", propNum("Page number (1-based)")}, {"perPage", propNum("Results per page (max 100)")}}, "owner", "repo"), readOnly: true,
		run: func(a map[string]any, u Urls) any {
			r := repoArgs(a)
			state := strings.ToLower(argText(a, "state", false))
			if state == "" {
				state = "open"
			}
			var list []*Pull
			pulls := r.SortedPulls()
			for k := len(pulls) - 1; k >= 0; k-- {
				if state == "all" || pulls[k].Issue.State == state {
					list = append(list, pulls[k])
				}
			}
			start, end := pageSlice(len(list), a)
			out := []any{}
			for _, p := range list[start:end] {
				out = append(out, pullView(r, p, u, false))
			}
			return Obj{{"pull_requests", out}}
		}},
	{name: "get_pull_request_files", title: "Get pull request files", description: "List the files a pull request changes, with their status and line counts.",
		schema: toolSchema(Obj{{"owner", ownerProp}, {"repo", repoProp}, {"pull_number", propNum("Pull request number")}}, "owner", "repo", "pull_number"), readOnly: true,
		run: func(a map[string]any, _ Urls) any {
			r := repoArgs(a)
			n, _ := argInt(a, "pull_number", true)
			files := []any{}
			for _, fl := range pullFiles(r, requirePull(r, n)) {
				files = append(files, Obj{{"filename", fl.Filename}, {"status", fl.Status}, {"additions", fl.Additions}, {"deletions", fl.Deletions}, {"changes", fl.Changes}, {"sha", fl.Sha}})
			}
			return Obj{{"files", files}}
		}},
	{name: "merge_pull_request", title: "Merge pull request", description: "Merge a pull request (merge, squash or rebase; default squash). A conflicted pull request is refused.",
		schema: toolSchema(Obj{{"owner", ownerProp}, {"repo", repoProp}, {"pull_number", propNum("Pull request number")}, {"merge_method", propEnum("Merge method", "merge", "squash", "rebase")}, {"commit_title", propStr("Title of the merge commit")}, {"commit_message", propStr("Extra detail for the merge commit")}}, "owner", "repo", "pull_number"),
		run: func(a map[string]any, _ Urls) any {
			r := repoArgs(a)
			n, _ := argInt(a, "pull_number", true)
			p := requirePull(r, n)
			method := argText(a, "merge_method", false)
			if method == "" {
				method = "squash"
			}
			sha, msg := mergePull(r, p, method, argText(a, "commit_title", false), argText(a, "commit_message", false), "")
			return Obj{{"sha", sha}, {"merged", true}, {"message", msg}, {"number", p.Number}}
		}},
}

// toolListJSON is the tools/list answer, serialised once: every call is byte-for-byte the same.
var toolListJSON = func() RawJSON {
	tools := []any{}
	for _, t := range mcpTools {
		tools = append(tools, Obj{{"name", t.name}, {"title", t.title}, {"description", t.description}, {"inputSchema", t.schema}, {"annotations", Obj{{"title", t.title}, {"readOnlyHint", t.readOnly}}}})
	}
	return RawJSON(toJSON(Obj{{"tools", tools}}))
}()

func findMCPTool(name string) *mcpTool {
	for _, t := range mcpTools {
		if t.name == name {
			return t
		}
	}
	return nil
}

func rpcError(id any, code int, message string) Obj {
	return Obj{{"jsonrpc", "2.0"}, {"id", id}, {"error", Obj{{"code", code}, {"message", message}}}}
}

func rpcResult(id any, result any) Obj {
	return Obj{{"jsonrpc", "2.0"}, {"id", id}, {"result", result}}
}

func toolResult(value any, isError bool) Obj {
	var text string
	if s, ok := value.(string); ok {
		text = s
	} else {
		text = string(toJSON(value))
	}
	result := Obj{{"content", []any{Obj{{"type", "text"}, {"text", text}}}}, {"isError", isError}}
	if o, ok := value.(Obj); ok && !isError {
		result = append(result, KV{"structuredContent", o})
	}
	return result
}

func callTool(params map[string]any, u Urls, id any) (out Obj) {
	name, _ := params["name"].(string)
	if name == "" {
		return rpcError(id, -32602, "Invalid params: tools/call needs a tool name")
	}
	tool := findMCPTool(name)
	if tool == nil {
		return rpcError(id, -32602, "Unknown tool: "+name)
	}
	args := map[string]any{}
	if raw, present := params["arguments"]; present && raw != nil {
		m, ok := raw.(map[string]any)
		if !ok {
			return rpcError(id, -32602, "Invalid params: arguments must be an object")
		}
		args = m
	}
	defer func() {
		if r := recover(); r != nil {
			switch e := r.(type) {
			case *invalidArguments:
				out = rpcError(id, -32602, "Invalid params: "+e.msg)
			case *HTTPError:
				detail := ""
				if errsV, ok := e.Extra.Get("errors"); ok {
					if list, isList := errsV.([]any); isList {
						var parts []string
						for _, item := range list {
							if o, isObj := item.(Obj); isObj {
								if m, has := o.Get("message"); has {
									if s, isStr := m.(string); isStr && s != "" {
										parts = append(parts, s)
									}
								}
							}
						}
						if len(parts) > 0 {
							detail = " (" + strings.Join(parts, "; ") + ")"
						}
					}
				}
				out = rpcResult(id, toolResult(fmt.Sprintf("%d %s%s", e.Status, e.Message, detail), true))
			default:
				out = rpcResult(id, toolResult(fmt.Sprint(r), true))
			}
		}
	}()
	return rpcResult(id, toolResult(tool.run(args, u), false))
}

// dispatchMCP answers one JSON-RPC message; nil for a notification.
func dispatchMCP(message any, u Urls) Obj {
	msg, ok := message.(map[string]any)
	if !ok {
		return rpcError(nil, -32600, "Invalid Request")
	}
	var id any
	hasID := false
	switch v := msg["id"].(type) {
	case string:
		id, hasID = v, true
	case float64:
		id, hasID = v, true
	}
	method, isStr := msg["method"].(string)
	if msg["jsonrpc"] != "2.0" || !isStr {
		_, hasResult := msg["result"]
		_, hasError := msg["error"]
		if !hasID && (hasResult || hasError) {
			return nil
		}
		return rpcError(id, -32600, "Invalid Request")
	}
	if !hasID {
		return nil
	}
	params, _ := msg["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}
	switch method {
	case "server/discover":
		return rpcResult(id, Obj{{"supportedVersions", mcpSupported}, {"capabilities", mcpCapabilities}, {"serverInfo", mcpServerInfo}, {"instructions", mcpInstructions}, {"_meta", Obj{{"io.modelcontextprotocol/serverInfo", mcpServerInfo}}}})
	case "initialize":
		asked, _ := params["protocolVersion"].(string)
		version := mcpHandshakeDefault
		for _, v := range mcpSupported {
			if v == asked {
				version = asked
			}
		}
		return rpcResult(id, Obj{{"protocolVersion", version}, {"capabilities", mcpCapabilities}, {"serverInfo", mcpServerInfo}, {"instructions", mcpInstructions}})
	case "ping", "logging/setLevel":
		return rpcResult(id, Obj{})
	case "tools/list":
		return rpcResult(id, toolListJSON)
	case "tools/call":
		return callTool(params, u, id)
	case "resources/list":
		return rpcResult(id, Obj{{"resources", []any{}}})
	case "resources/templates/list":
		return rpcResult(id, Obj{{"resourceTemplates", []any{}}})
	case "prompts/list":
		return rpcResult(id, Obj{{"prompts", []any{}}})
	}
	return rpcError(id, -32601, "Method not found: "+method)
}

func mcpResp(status int, body any) *Response {
	resp := &Response{Status: status, Headers: [][2]string{{"x-github-request-id", nextRequestID()}}}
	if body != nil {
		resp.Headers = append([][2]string{{"content-type", "application/json"}}, resp.Headers...)
		resp.Body = toJSON(body)
	}
	return resp
}

func handleMCP(r *Request) *Response {
	if !isAuthorized(r) {
		return mcpResp(401, rpcError(nil, -32001, "Unauthorized: a bearer token is required")).Add("www-authenticate", `Bearer realm="git-forge"`)
	}
	if r.Method != "POST" {
		return mcpResp(405, rpcError(nil, -32000, "Method not allowed: POST JSON-RPC messages to this endpoint")).Add("allow", "POST")
	}
	message, err := parseJSON(r.Body)
	if err != nil {
		return mcpResp(400, rpcError(nil, -32700, "Parse error"))
	}
	u := urlsOf(r)
	if batch, ok := message.([]any); ok {
		if len(batch) == 0 {
			return mcpResp(400, rpcError(nil, -32600, "Invalid Request: empty batch"))
		}
		answers := []any{}
		for _, m := range batch {
			if a := dispatchMCP(m, u); a != nil {
				answers = append(answers, a)
			}
		}
		if len(answers) == 0 {
			return mcpResp(202, nil)
		}
		return mcpResp(200, answers)
	}
	answer := dispatchMCP(message, u)
	if answer == nil {
		return mcpResp(202, nil)
	}
	status := 200
	if e, ok := answer.Get("error"); ok {
		if code, _ := e.(Obj).Get("code"); code == -32600 {
			status = 400
		}
	}
	return mcpResp(status, answer)
}
