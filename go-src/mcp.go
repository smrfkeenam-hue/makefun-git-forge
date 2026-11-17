// MCP: JSON-RPC 2.0 over Streamable HTTP (plain JSON answers, no SSE stream).
// The modern era only: the client probes server/discover, negotiates revision
// 2026-07-28, and calls tools/list and tools/call. Every tool acts on the
// same store as REST, with the same stable ids.
//
// Wire rules: auth (Bearer $MAKEFUN_SHOP_TOKEN) is decided before anything
// else — a request without it is HTTP 401, even for a tool that does not
// exist. A body that is not JSON is HTTP 400 with JSON-RPC -32700. An unknown
// tool is -32602 (never HTTP 500). A call missing a required argument is
// -32602. Every request answers well inside the client's 3 s timeout.
//
// tools/list is byte-identical across calls: the tool definitions are built
// once, in a fixed order, from one literal list.

package main

import (
	"math"
	"strings"
)

// --------------------------------------------------------------------------- tools

// mcpProp is one input-schema property: its JSON type and description.
type mcpProp struct {
	typ         string
	description string
}

type mcpToolDef struct {
	name        string
	description string
	properties  map[string]mcpProp
	required    []string
}

// mcpTools is the whole tool surface, in the fixed order tools/list returns.
var mcpTools = []mcpToolDef{
	{
		name:        "create_repo",
		description: "Create a repository owned by the authenticated user.",
		properties: map[string]mcpProp{
			"owner":       {"string", "The login that will own the repository."},
			"name":        {"string", "The repository name."},
			"description": {"string", "A short description of the repository."},
			"private":     {"boolean", "Whether the repository is private."},
		},
		required: []string{"name"},
	},
	{
		name:        "get_repo",
		description: "Read one repository by owner and name.",
		properties: map[string]mcpProp{
			"owner": {"string", "The repository owner login."},
			"repo":  {"string", "The repository name."},
		},
		required: []string{"owner", "repo"},
	},
	{
		name:        "create_issue",
		description: "Create an issue in a repository.",
		properties: map[string]mcpProp{
			"owner": {"string", "The repository owner login."},
			"repo":  {"string", "The repository name."},
			"title": {"string", "The issue title."},
			"body":  {"string", "The issue body."},
		},
		required: []string{"owner", "repo", "title"},
	},
	{
		name:        "list_issues",
		description: "List the issues of a repository, optionally filtered by state.",
		properties: map[string]mcpProp{
			"owner": {"string", "The repository owner login."},
			"repo":  {"string", "The repository name."},
			"state": {"string", "Issue state to list: open, closed, or all."},
		},
		required: []string{"owner", "repo"},
	},
	{
		name:        "create_pull_request",
		description: "Open a pull request from a head branch onto a base branch.",
		properties: map[string]mcpProp{
			"owner": {"string", "The repository owner login."},
			"repo":  {"string", "The repository name."},
			"title": {"string", "The pull request title."},
			"body":  {"string", "The pull request body."},
			"head":  {"string", "The head branch name."},
			"base":  {"string", "The base branch name."},
		},
		required: []string{"owner", "repo", "title", "head", "base"},
	},
	{
		name:        "merge_pull_request",
		description: "Squash-merge a pull request onto its base branch.",
		properties: map[string]mcpProp{
			"owner":       {"string", "The repository owner login."},
			"repo":        {"string", "The repository name."},
			"pull_number": {"number", "The pull request number."},
		},
		required: []string{"owner", "repo", "pull_number"},
	},
}

func mcpServerInfo() map[string]any {
	return map[string]any{"name": "makefun-forge", "version": "1.0.0"}
}

func mcpToolShape(def mcpToolDef) map[string]any {
	properties := map[string]any{}
	for k, v := range def.properties {
		properties[k] = map[string]any{"type": v.typ, "description": v.description}
	}
	required := make([]any, len(def.required))
	for i, name := range def.required {
		required[i] = name
	}
	return map[string]any{
		"name":        def.name,
		"description": def.description,
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": properties,
			"required":   required,
		},
	}
}

// --------------------------------------------------------------------------- replies

type mcpReply struct {
	Status int
	JSON   map[string]any
}

func mcpResult(id any, result map[string]any) mcpReply {
	return mcpReply{Status: 200, JSON: map[string]any{"jsonrpc": "2.0", "id": id, "result": result}}
}

func mcpError(id any, code int64, message string) mcpReply {
	return mcpReply{Status: 200, JSON: map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": message},
	}}
}

// mcpToolResult is a tool result: a text block carrying the JSON object, and
// the same object structured.
func mcpToolResult(id any, value any) mcpReply {
	text := marshalJSON(value)
	return mcpResult(id, map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": string(text)}},
		"structuredContent": value,
	})
}

func mcpToolError(id any, message string) mcpReply {
	return mcpResult(id, map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": message}},
		"structuredContent": map[string]any{"error": message},
		"isError":           true,
	})
}

// --------------------------------------------------------------------------- shapes

// mcpRepoShape is the small repository shape the MCP tools return (not the
// full REST shape).
func mcpRepoShape(owner string, repo *Repo) map[string]any {
	return map[string]any{
		"id":             repo.ID,
		"name":           repo.Name,
		"full_name":      owner + "/" + repo.Name,
		"owner":          map[string]any{"login": owner},
		"private":        repo.Private,
		"description":    repo.Description,
		"default_branch": repo.DefaultBranch,
	}
}

func mcpIssueShape(owner, repoName string, issue *Issue) map[string]any {
	return map[string]any{
		"id":        issue.ID,
		"number":    issue.Number,
		"title":     issue.Title,
		"body":      issue.Body,
		"state":     issue.State,
		"full_name": owner + "/" + repoName,
	}
}

// --------------------------------------------------------------------------- handler

// HandleMCP serves POST /mcp. body is the raw request body.
func HandleMCP(store *Store, body []byte) mcpReply {
	parsed, err := parseJSONBody(body)
	if err != nil {
		return mcpReply{Status: 400, JSON: map[string]any{
			"jsonrpc": "2.0",
			"id":      nil,
			"error":   map[string]any{"code": -32700, "message": "Parse error"},
		}}
	}
	record := asRecord(parsed)
	// The id is a string or a number, else null (a boolean or object id
	// becomes null, it is not a notification).
	var id any
	switch v := record["id"].(type) {
	case string, float64, int64, int:
		id = v
	default:
		id = nil
	}
	method, _ := record["method"].(string)
	params := asRecord(record["params"])

	// A notification carries no id key at all and expects no answer.
	if _, hasID := record["id"]; !hasID {
		return mcpReply{Status: 200, JSON: map[string]any{}}
	}

	switch method {
	case "server/discover":
		return mcpResult(id, map[string]any{
			"supportedVersions": []any{"2026-07-28"},
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        mcpServerInfo(),
			"_meta":             map[string]any{"io.modelcontextprotocol/serverInfo": mcpServerInfo()},
		})
	case "initialize":
		// The offered version is echoed when it is a string (even an empty
		// one); otherwise the default.
		protocolVersion := "2026-07-28"
		if offered, ok := params["protocolVersion"].(string); ok {
			protocolVersion = offered
		}
		return mcpResult(id, map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      mcpServerInfo(),
		})
	case "tools/list":
		tools := make([]any, len(mcpTools))
		for i, def := range mcpTools {
			tools[i] = mcpToolShape(def)
		}
		return mcpResult(id, map[string]any{"tools": tools})
	case "tools/call":
		toolName, _ := params["name"].(string)
		args := asRecord(params["arguments"])
		return mcpCallTool(store, id, toolName, args)
	case "ping":
		return mcpResult(id, map[string]any{})
	default:
		return mcpError(id, -32601, "method not found: "+method)
	}
}

func mcpCallTool(store *Store, id any, toolName string, args map[string]any) mcpReply {
	var def *mcpToolDef
	for i := range mcpTools {
		if mcpTools[i].name == toolName {
			def = &mcpTools[i]
			break
		}
	}
	if def == nil {
		return mcpError(id, -32602, "unknown tool "+toolName)
	}
	var missing []string
	for _, required := range def.required {
		value, ok := args[required]
		if !ok || value == nil || value == "" {
			missing = append(missing, required)
		}
	}
	if len(missing) > 0 {
		return mcpError(id, -32602, "missing required argument(s): "+strings.Join(missing, ", "))
	}
	// owner defaults to the viewer's login when it is not a string.
	owner, _ := args["owner"].(string)
	if _, ok := args["owner"].(string); !ok {
		owner = store.ViewerLogin()
	}
	repoName, _ := args["repo"].(string)
	if _, ok := args["repo"].(string); !ok {
		if name, ok := args["name"].(string); ok {
			repoName = name
		}
	}

	switch toolName {
	case "create_repo":
		private, _ := args["private"].(bool)
		description, _ := args["description"].(string)
		repo := store.CreateRepo(repoName, private, description)
		if repo == nil {
			return mcpToolError(id, "repository "+repoName+" already exists")
		}
		return mcpToolResult(id, mcpRepoShape(owner, repo))
	case "get_repo":
		repo := store.GetRepo(owner, repoName)
		if repo == nil {
			return mcpToolError(id, "repository "+owner+"/"+repoName+" not found")
		}
		return mcpToolResult(id, mcpRepoShape(owner, repo))
	case "create_issue":
		repo := store.GetRepo(owner, repoName)
		if repo == nil {
			return mcpToolError(id, "repository "+owner+"/"+repoName+" not found")
		}
		// Labels are not passed: the MCP create_issue has no labels argument.
		body, _ := args["body"].(string)
		issue := store.CreateIssue(repo, jsString(args["title"]), body, nil)
		if issue == nil {
			return mcpToolError(id, "could not create the issue")
		}
		return mcpToolResult(id, mcpIssueShape(owner, repoName, issue))
	case "list_issues":
		repo := store.GetRepo(owner, repoName)
		if repo == nil {
			return mcpToolError(id, "repository "+owner+"/"+repoName+" not found")
		}
		state, _ := args["state"].(string)
		if _, ok := args["state"].(string); !ok {
			state = "all"
		}
		issues := store.ListIssues(repo, state, "", "")
		items := make([]any, len(issues))
		for i, issue := range issues {
			items[i] = map[string]any{
				"id":     issue.ID,
				"number": issue.Number,
				"title":  issue.Title,
				"body":   issue.Body,
				"state":  issue.State,
			}
		}
		return mcpToolResult(id, map[string]any{"issues": items, "total_count": int64(len(issues))})
	case "create_pull_request":
		repo := store.GetRepo(owner, repoName)
		if repo == nil {
			return mcpToolError(id, "repository "+owner+"/"+repoName+" not found")
		}
		body, _ := args["body"].(string)
		base, _ := args["base"].(string)
		if _, ok := args["base"].(string); !ok {
			base = repo.DefaultBranch
		}
		pull := store.CreatePullRequest(repo, jsString(args["title"]), body, jsString(args["head"]), base)
		if pull == nil {
			return mcpToolError(id, "head or base branch does not exist")
		}
		return mcpToolResult(id, map[string]any{
			"id":          pull.ID,
			"number":      pull.Number,
			"pull_number": pull.Number,
			"title":       pull.Title,
			"body":        pull.Body,
			"state":       pull.State,
			"head":        pull.HeadRef,
			"base":        pull.BaseRef,
			"full_name":   owner + "/" + repoName,
		})
	case "merge_pull_request":
		repo := store.GetRepo(owner, repoName)
		if repo == nil {
			return mcpToolError(id, "repository "+owner+"/"+repoName+" not found")
		}
		var pullNumber float64
		switch n := args["pull_number"].(type) {
		case float64:
			pullNumber = n
		case int64:
			pullNumber = float64(n)
		case int:
			pullNumber = float64(n)
		default:
			// parseInt(String(args.pull_number ?? ''), 10): lenient prefix
			// parsing, NaN when nothing parses.
			parsed, ok := jsParseInt(jsString(args["pull_number"]))
			if !ok {
				return mcpError(id, -32602, "pull_number must be a number")
			}
			pullNumber = float64(parsed)
		}
		if math.IsNaN(pullNumber) || math.IsInf(pullNumber, 0) {
			return mcpError(id, -32602, "pull_number must be a number")
		}
		// A non-integer number matches no pull (JS getPull compares with
		// ===, so 5.7 never equals a pull number).
		number, ok := toInt(pullNumber)
		if !ok {
			number = -1
		}
		merged := store.MergePull(repo, number)
		if merged.Error != "" {
			return mcpToolError(id, "could not merge the pull request: "+merged.Error)
		}
		return mcpToolResult(id, map[string]any{
			"number":      pullNumber,
			"pull_number": pullNumber,
			"merged":      true,
			"sha":         merged.SHA,
		})
	}
	return mcpError(id, -32602, "unknown tool "+toolName)
}

// labelNames extracts a []string of label names from a REST argument value (an
// array of strings, or anything else -> nil). Shared with rest.go.
func labelNames(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
