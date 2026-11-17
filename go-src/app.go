// App: the router. It owns the store, the token, and the dispatch order, and
// it is the only part of the Go core that knows about HTTP paths:
//
//	GET /healthz            200 {"ok":true}, no auth
//	/api/graphql            GraphQL v4 (auth first, then method)
//	/mcp                    MCP JSON-RPC (auth first, then method)
//	/api/v3/*               REST v3 (root and /meta need no auth)
//	/{owner}/{repo}.git/*   git smart HTTP (info/refs, upload-pack, receive-pack)
//
// Anything else is a 404 JSON reply. Auth is Bearer $MAKEFUN_SHOP_TOKEN; an
// empty token authenticates nothing (every authed route is 401). Dispatch
// runs under one mutex, so the store sees the same serialized order of
// operations the single-threaded Node server did. Every JSON reply carries
// content-type "application/json; charset=utf-8", including errors and the
// raw (pre-built) REST replies.

package main

import (
	"net/url"
	"os"
	"strings"
	"sync"
)

// JSONType is the content type every JSON reply carries.
const JSONType = "application/json; charset=utf-8"

// App is the forge: a store, a token, and a dispatch mutex.
type App struct {
	store *Store
	token string
	mu    sync.Mutex
}

// NewApp creates the app: a fresh store and the token read once from the
// environment.
func NewApp() *App {
	return &App{
		store: NewStore(),
		token: os.Getenv("MAKEFUN_SHOP_TOKEN"),
	}
}

// Handler returns the HTTP handler for this app (serialized dispatch).
func (a *App) Handler() Handler {
	return func(req *HttpRequest) HttpResponse {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.dispatch(req)
	}
}

// Store exposes the store (for tests).
func (a *App) Store() *Store { return a.store }

// segmentsOf splits a URL path into segments, dropping empties, percent-
// decoding each leniently (a segment that fails to decode is kept raw).
func segmentsOf(path string) []string {
	var out []string
	for _, part := range strings.Split(path, "/") {
		if part == "" {
			continue
		}
		if !strings.Contains(part, "%") {
			out = append(out, part)
			continue
		}
		decoded, err := url.PathUnescape(part)
		if err != nil {
			out = append(out, part)
		} else {
			out = append(out, decoded)
		}
	}
	return out
}

// queryParam returns one query parameter, with a fast path for a lone
// "service=<name>" query (the git info/refs form).
func queryParam(query, name string) string {
	if query == "" {
		return ""
	}
	prefix := name + "="
	if strings.HasPrefix(query, prefix) && !strings.Contains(query, "&") {
		value := query[len(prefix):]
		if !strings.Contains(value, "%") {
			return value
		}
		if decoded, err := url.PathUnescape(value); err == nil {
			return decoded
		}
		return value
	}
	values, _ := url.ParseQuery(query)
	return values.Get(name)
}

// splitTarget splits a raw request target into its path and query parts. A
// '#' truncates the target (a fragment never reaches the server).
func splitTarget(target string) (path, query string) {
	if i := strings.Index(target, "#"); i >= 0 {
		target = target[:i]
	}
	if i := strings.Index(target, "?"); i >= 0 {
		return target[:i], target[i+1:]
	}
	return target, ""
}

func jsonReply(status int, value any) HttpResponse {
	body := marshalJSON(value)
	body = append(body, '\n')
	return HttpResponse{
		Status:  status,
		Headers: [][2]string{{"content-type", JSONType}},
		Body:    body,
	}
}

func restToHTTP(reply RestReply) HttpResponse {
	if reply.Raw != nil {
		return HttpResponse{
			Status:  reply.Status,
			Headers: [][2]string{{"content-type", JSONType}},
			Body:    reply.Raw,
		}
	}
	headers := [][2]string{{"content-type", JSONType}}
	headers = append(headers, reply.Headers...)
	body := marshalJSON(reply.JSON)
	body = append(body, '\n')
	return HttpResponse{Status: reply.Status, Headers: headers, Body: body}
}

func mcpToHTTP(reply mcpReply) HttpResponse {
	return jsonReply(reply.Status, reply.JSON)
}

func gitToHTTP(resp GitResponse) HttpResponse {
	return HttpResponse{Status: resp.Status, Headers: resp.Headers, Body: resp.Body}
}

// authorized reports whether the request carries the Bearer token. An empty
// token authenticates nothing.
func (a *App) authorized(req *HttpRequest) bool {
	if a.token == "" {
		return false
	}
	return req.Headers["authorization"] == "Bearer "+a.token
}

func unauthorized() HttpResponse {
	return jsonReply(401, map[string]any{"message": "Requires authentication"})
}

func methodNotAllowed() HttpResponse {
	return jsonReply(405, map[string]any{"message": "Method Not Allowed"})
}

func notFound() HttpResponse {
	return jsonReply(404, map[string]any{"message": "Not Found"})
}

// dispatch routes one request. The order matters: healthz, GraphQL, MCP,
// REST, then git; auth is checked before the method on GraphQL and MCP.
func (a *App) dispatch(req *HttpRequest) HttpResponse {
	path, query := splitTarget(req.Target)
	segments := segmentsOf(path)

	// The boot probe: GET only, no token, always 200.
	if req.Method == "GET" && len(segments) == 1 && segments[0] == "healthz" {
		return jsonReply(200, map[string]any{"ok": true})
	}

	// GraphQL v4: auth first, then the method.
	if len(segments) == 2 && segments[0] == "api" && segments[1] == "graphql" {
		if !a.authorized(req) {
			return unauthorized()
		}
		if req.Method != "POST" {
			return methodNotAllowed()
		}
		return restToHTTP(HandleGraphQL(a.store, req.Body))
	}

	// MCP: auth first, then the method. DELETE is a session teardown and
	// answers {}.
	if len(segments) == 1 && segments[0] == "mcp" {
		if !a.authorized(req) {
			return unauthorized()
		}
		if req.Method == "DELETE" {
			return jsonReply(200, map[string]any{})
		}
		if req.Method != "POST" {
			return methodNotAllowed()
		}
		return mcpToHTTP(HandleMCP(a.store, req.Body))
	}

	// REST v3: the root and /meta need no auth.
	if len(segments) >= 2 && segments[0] == "api" && segments[1] == "v3" {
		restSegments := segments[2:]
		needsAuth := !(len(restSegments) == 0 || (len(restSegments) == 1 && restSegments[0] == "meta"))
		if needsAuth && !a.authorized(req) {
			return unauthorized()
		}
		host := req.Headers["host"]
		if host == "" {
			host = "localhost"
		}
		restReq := &RestRequest{
			Method:   req.Method,
			Segments: restSegments,
			Query:    parseQueryString(query),
			Body:     req.Body,
			Host:     host,
		}
		reply := HandleRest(a.store, restReq)
		if reply.Status == 204 {
			return HttpResponse{Status: 204, Headers: nil, Body: nil}
		}
		return restToHTTP(reply)
	}

	// Git smart HTTP: /{owner}/{repo}.git/info/refs | git-upload-pack |
	// git-receive-pack. A wrong method or an unknown service is a 404, not
	// a 405; extra trailing segments are ignored, as in the Node router.
	if len(segments) >= 3 && strings.HasSuffix(segments[1], ".git") {
		if !a.authorized(req) {
			return unauthorized()
		}
		owner := segments[0]
		repoName := segments[1][:len(segments[1])-len(".git")]
		service := segments[2]
		gitProtocol := req.Headers["git-protocol"]
		if service == "info" && len(segments) > 3 && segments[3] == "refs" && req.Method == "GET" {
			return gitToHTTP(HandleInfoRefs(a.store, owner, repoName, queryParam(query, "service"), gitProtocol))
		}
		if service == "git-upload-pack" && req.Method == "POST" {
			return gitToHTTP(HandleUploadPack(a.store, owner, repoName, req.Body, gitProtocol))
		}
		if service == "git-receive-pack" && req.Method == "POST" {
			return gitToHTTP(HandleReceivePack(a.store, owner, repoName, req.Body))
		}
		return notFound()
	}

	// Anything else: a JSON 404, never HTML.
	return notFound()
}
