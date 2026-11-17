package main

// Opaque, stable global ids (GitHub's node_id / GraphQL id): a type prefix plus base64url of the kind and
// database id. They never equal the plain number and decode back for node(id:).

import (
	"encoding/base64"
	"fmt"
	"strings"
)

var nodePrefixes = []struct{ kind, prefix string }{
	{"Repository", "R_kgDO"},
	{"Issue", "I_kwDO"},
	{"PullRequest", "PR_kwDO"},
	{"IssueComment", "IC_kwDO"},
	{"User", "U_kgDO"},
	{"Label", "LA_kwDO"},
	{"Commit", "C_kwDO"},
	{"Ref", "REF_kwDO"},
}

func nodeID(kind string, id any) string {
	for _, p := range nodePrefixes {
		if p.kind == kind {
			return p.prefix + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%v", kind, id)))
		}
	}
	return ""
}

func decodeNodeID(value string) (string, string, bool) {
	for _, p := range nodePrefixes {
		if !strings.HasPrefix(value, p.prefix) {
			continue
		}
		raw, err := base64.RawURLEncoding.DecodeString(value[len(p.prefix):])
		if err != nil {
			continue
		}
		decoded := string(raw)
		colon := strings.IndexByte(decoded, ':')
		if colon < 0 || decoded[:colon] != p.kind {
			continue
		}
		return p.kind, decoded[colon+1:], true
	}
	return "", "", false
}
