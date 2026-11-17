package main

// git-forge in Go: the same forge as the TypeScript one (app.ts and src/), served from one process.
//
// main.ts starts this binary, reads the "PORT <n>" line it prints, and registers that port with the
// trial env; it keeps doing /register and /done itself. Run by hand, FORGE_PORT picks the port (0 or unset:
// an ephemeral one). Every request that is not a GET or HEAD may change the store, so it bumps the
// mutation version the response caches compare against.

import (
	"fmt"
	"net"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

var mutationVersion uint64

func timeParse(s string) (float64, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0, err
	}
	return float64(t.Unix()), nil
}

func route(r *Request) *Response {
	path := r.Path
	reads := r.Method == "GET" || r.Method == "HEAD"
	if reads {
		if resp := fastIssueGet(r); resp != nil {
			return resp
		}
	} else {
		mutationVersion++
		defer func() { mutationVersion++ }()
	}
	switch {
	case path == "/healthz":
		return jsonResp(200, Obj{{"ok", true}})
	case path == "/api/v3" || strings.HasPrefix(path, "/api/v3/"):
		return handleRest(r)
	case path == "/mcp" || path == "/mcp/":
		return handleMCP(r)
	case path == "/api/graphql" || path == "/graphql":
		return handleGraphQL(r)
	case isGitPath(path):
		if resp := handleGit(r); resp != nil {
			return resp
		}
	}
	return errorResp(404, "Not Found", nil)
}

func main() {
	runtime.GOMAXPROCS(2)
	debug.SetMemoryLimit(700 << 20)
	debug.SetGCPercent(400)
	loadConfig()
	githubSchema = buildGitHubSchema()

	port := 0
	if p, err := strconv.Atoi(strings.TrimSpace(os.Getenv("FORGE_PORT"))); err == nil && p > 0 {
		port = p
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "[forge-go] listen: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("PORT %d\n", ln.Addr().(*net.TCPAddr).Port)
	serve(ln, route)
}
