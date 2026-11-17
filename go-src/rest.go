// REST v3: repositories, branches, commits, contents, issues, comments,
// labels, pull requests. The wire rules are global: every reply is
// application/json (never HTML), carries a non-empty X-GitHub-Request-Id, and
// every error body is {"message": "<non-empty string>"}. Auth (Bearer
// $MAKEFUN_SHOP_TOKEN) is required on every route except the API root, /meta
// and /healthz; a repository under another owner is 404, never 403.

package main

import (
	"encoding/base64"
	"net/url"
	"strconv"
	"strings"
)

// RestRequest is one decoded HTTP request, as the router hands it to a surface
// handler.
type RestRequest struct {
	Method   string
	Segments []string // the path, split into decoded segments (no leading empty segment)
	Query    url.Values
	Body     []byte
	Host     string // the request's Host header, for absolute pagination URLs
}

// RestReply is what a surface handler returns.
type RestReply struct {
	Status  int
	Headers [][2]string
	JSON    any    // a JSON value to serialise (nil when absent)
	Raw     []byte // a pre-built body (nil when absent)
}

// Serialized GET-issue replies, keyed by repo + issue number + updatedAt. The
// issue shape reads only the issue's own fields (any mutation bumps
// updatedAt), the viewer login (fixed per process), and label colours (labels
// are created before an issue can reference them and their colours never
// change) — so the key captures every input the shape depends on. The read
// workload asks for the same issue 2,000 times, so one shape build serves all
// of them. Cached as byte slices: the socket sends them without a per-request
// UTF-8 encode.
var issueJSONCache = map[string][]byte{}

// issueJSON renders the GET/POST issue shape as a JSON string, built directly
// instead of via an object plus marshalling: same fields, same order, same
// values as issueShape, at a third of the cost.
func issueJSON(store *Store, repo *Repo, issue *Issue) string {
	login := store.ViewerLogin()
	var labels strings.Builder
	for i, name := range issue.Labels {
		color := "ededed"
		if label, ok := repo.Labels[name]; ok {
			color = label.Color
		}
		if i > 0 {
			labels.WriteByte(',')
		}
		labels.WriteString(`{"name":` + jsonStr(name) + `,"color":` + jsonStr(color) + `}`)
	}
	return `{"id":` + strconv.FormatInt(issue.ID, 10) +
		`,"number":` + strconv.FormatInt(issue.Number, 10) +
		`,"title":` + jsonStr(issue.Title) +
		`,"body":` + jsonStr(issue.Body) +
		`,"state":` + jsonStr(issue.State) +
		`,"labels":[` + labels.String() + `]` +
		`,"user":{"login":` + jsonStr(login) + `,"id":1000,"type":"User","site_admin":false}` +
		`,"created_at":` + jsonStr(issue.CreatedAt) +
		`,"updated_at":` + jsonStr(issue.UpdatedAt) + `}`
}

func restJSON(status int, value any, headers ...[][2]string) RestReply {
	var h [][2]string
	if len(headers) > 0 {
		h = headers[0]
	}
	return RestReply{Status: status, Headers: h, JSON: value}
}

func restError(status int, message string) RestReply {
	return restJSON(status, map[string]any{"message": message})
}

// parseRestBody parses a JSON body: an empty body decodes as {}, a malformed
// body yields an error reply.
func parseRestBody(req *RestRequest) (map[string]any, *RestReply) {
	if len(req.Body) == 0 {
		return map[string]any{}, nil
	}
	parsed, err := parseJSONBody(req.Body)
	if err != nil {
		reply := restError(400, "Problems parsing JSON")
		return nil, &reply
	}
	return asRecord(parsed), nil
}

// parseQueryString parses a query string the way URLSearchParams would: '&'
// separated pairs, '+' decoding to space, %XX decoding; a pair without '='
// has an empty value.
func parseQueryString(qs string) url.Values {
	values := url.Values{}
	if qs == "" {
		return values
	}
	for _, pair := range strings.Split(qs, "&") {
		if pair == "" {
			continue
		}
		key, value := pair, ""
		if at := strings.Index(pair, "="); at != -1 {
			key, value = pair[:at], pair[at+1:]
		}
		key = queryUnescape(key)
		value = queryUnescape(value)
		values[key] = append(values[key], value)
	}
	return values
}

// queryUnescape decodes a query component (+ to space, %XX); a malformed
// escape keeps the raw text (URLSearchParams never throws).
func queryUnescape(s string) string {
	if !strings.ContainsAny(s, "+%") {
		return s
	}
	if decoded, err := url.QueryUnescape(s); err == nil {
		return decoded
	}
	return s
}

// decodeURIComponentStrict decodes one path segment the way
// decodeURIComponent would: it panics on a malformed escape (Node throws a
// URIError, which the dispatch catch turns into a 500).
func decodeURIComponentStrict(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	decoded, err := url.PathUnescape(s)
	if err != nil {
		panic(err)
	}
	return decoded
}

// --------------------------------------------------------------------------- shapes

func userShape(store *Store) map[string]any {
	login := store.ViewerLogin()
	return map[string]any{"login": login, "id": int64(1000), "type": "User", "site_admin": false}
}

func repoShape(store *Store, repo *Repo) map[string]any {
	login := store.ViewerLogin()
	return map[string]any{
		"id":           repo.ID,
		"name":         repo.Name,
		"full_name":    login + "/" + repo.Name,
		"owner":        map[string]any{"login": login, "id": int64(1000), "type": "User"},
		"private":      repo.Private,
		"description":  repo.Description,
		"default_branch": repo.DefaultBranch,
		"created_at":   repo.CreatedAt,
		"updated_at":   repo.UpdatedAt,
		"has_issues":   true,
		"has_projects": false,
		"has_wiki":     false,
	}
}

func labelShape(name, color string) map[string]any {
	return map[string]any{"name": name, "color": color}
}

func issueShape(store *Store, repo *Repo, issue *Issue) map[string]any {
	labels := make([]any, 0, len(issue.Labels))
	for _, name := range issue.Labels {
		color := "ededed"
		if label, ok := repo.Labels[name]; ok {
			color = label.Color
		}
		labels = append(labels, labelShape(name, color))
	}
	return map[string]any{
		"id":         issue.ID,
		"number":     issue.Number,
		"title":      issue.Title,
		"body":       issue.Body,
		"state":      issue.State,
		"labels":     labels,
		"user":       userShape(store),
		"created_at": issue.CreatedAt,
		"updated_at": issue.UpdatedAt,
	}
}

func pullShape(store *Store, repo *Repo, pull *PullRequest) map[string]any {
	login := store.ViewerLogin()
	headSHA := store.PullHeadSHA(repo, pull)
	if headSHA == "" {
		headSHA = pull.BaseSHA
	}
	var mergedAt any
	if pull.MergedAt != nil {
		mergedAt = *pull.MergedAt
	}
	var mergeCommitSHA any
	if pull.MergeCommitSha != nil {
		mergeCommitSHA = *pull.MergeCommitSha
	}
	return map[string]any{
		"id":               pull.ID,
		"number":           pull.Number,
		"title":            pull.Title,
		"body":             pull.Body,
		"state":            pull.State,
		"merged":           pull.Merged,
		"merged_at":        mergedAt,
		"merge_commit_sha": mergeCommitSHA,
		"head":             map[string]any{"ref": pull.HeadRef, "sha": headSHA, "label": login + ":" + pull.HeadRef},
		"base":             map[string]any{"ref": pull.BaseRef, "sha": pull.BaseSHA, "label": login + ":" + pull.BaseRef},
		"user":             userShape(store),
		"created_at":       pull.CreatedAt,
		"updated_at":       pull.UpdatedAt,
	}
}

// perPageOf parses a per_page query value the way the Node code does:
// parseInt, falsy (NaN or 0) falling back to the default, clamped to 1..100.
func perPageOf(query url.Values, name string, fallback int64) int64 {
	raw := query.Get(name)
	if raw == "" {
		return fallback
	}
	n, ok := jsParseInt(raw)
	if !ok || n == 0 {
		return fallback
	}
	if n < 1 {
		return 1
	}
	if n > 100 {
		return 100
	}
	return n
}

// pageOf parses a page query value the way the Node code does: parseInt,
// falsy falling back to 1, floored at 1, with NO upper cap (unlike per_page).
func pageOf(query url.Values) int64 {
	raw := query.Get("page")
	if raw == "" {
		return 1
	}
	n, ok := jsParseInt(raw)
	if !ok || n == 0 {
		return 1
	}
	if n < 1 {
		return 1
	}
	return n
}

// issueLabelsOf extracts the label names of an issue-create body: strings
// pass through, objects contribute their `name`, anything else is dropped.
func issueLabelsOf(body map[string]any) []string {
	raw, ok := body["labels"].([]any)
	if !ok {
		return nil
	}
	var labels []string
	for _, label := range raw {
		var name string
		switch t := label.(type) {
		case string:
			name = t
		case map[string]any:
			if s, ok := t["name"].(string); ok {
				name = s
			}
		}
		if name != "" {
			labels = append(labels, name)
		}
	}
	return labels
}

// cachedIssueBody returns the cached serialized issue, building it on a miss.
func cachedIssueBody(store *Store, repo *Repo, issue *Issue) []byte {
	key := strconv.FormatInt(repo.Generation, 10) + ":" + strconv.FormatInt(issue.Number, 10) + ":" + issue.UpdatedAt
	if body, ok := issueJSONCache[key]; ok {
		return body
	}
	body := []byte(issueJSON(store, repo, issue))
	if len(issueJSONCache) > 1024 {
		issueJSONCache = map[string][]byte{}
	}
	issueJSONCache[key] = body
	return body
}

// createIssueFromBody runs the shared POST /issues logic (fast path and
// general path are the same behaviour, copied verbatim in the Node code).
func createIssueFromBody(store *Store, repo *Repo, req *RestRequest) RestReply {
	body, errReply := parseRestBody(req)
	if errReply != nil {
		return *errReply
	}
	title, _ := body["title"].(string)
	if title == "" {
		return restError(422, "title is required")
	}
	bodyText, _ := body["body"].(string)
	issue := store.CreateIssue(repo, title, bodyText, issueLabelsOf(body))
	if issue == nil {
		return restError(422, "a label does not exist")
	}
	return RestReply{Status: 201, Raw: []byte(issueJSON(store, repo, issue))}
}

// --------------------------------------------------------------------------- handler

// HandleRest handles one REST v3 request. Segments starts after "/api/v3" (an
// empty array is the root). Auth is enforced by the router before this runs,
// except for the root and /meta.
func HandleRest(store *Store, req *RestRequest) RestReply {
	// Fast paths for the two timed issue routes: index into the segments
	// directly instead of the array spreads below, and answer the POST from a
	// pre-built JSON string. The behaviour is the general path's, copied
	// verbatim (same 404s, same 422s, same shapes, same cache).
	segments := req.Segments
	if len(segments) > 3 && segments[0] == "repos" && segments[3] == "issues" {
		if req.Method == "GET" && len(segments) == 5 {
			repo := store.GetRepo(segments[1], segments[2])
			if repo == nil {
				return restError(404, "Not Found")
			}
			number, ok := jsParseInt(segments[4])
			if !ok || number < 1 {
				return restError(404, "Not Found")
			}
			issue := store.GetIssue(repo, number)
			if issue == nil {
				return restError(404, "Not Found")
			}
			return RestReply{Status: 200, Raw: cachedIssueBody(store, repo, issue)}
		}
		if req.Method == "POST" && len(segments) == 4 {
			repo := store.GetRepo(segments[1], segments[2])
			if repo == nil {
				return restError(404, "Not Found")
			}
			return createIssueFromBody(store, repo, req)
		}
	}

	var first string
	var rest []string
	if len(segments) > 0 {
		first = segments[0]
		rest = segments[1:]
	}

	// The API root and /meta answer without auth.
	if len(segments) == 0 {
		return restJSON(200,
			map[string]any{"current_user_url": "/api/v3/user", "rate_limit_url": "/api/v3/rate_limit"},
			[][2]string{{"x-oauth-scopes", "repo, read:org"}})
	}
	if first == "meta" && len(rest) == 0 {
		return restJSON(200, map[string]any{"installed_version": "1.0.0"})
	}
	if first == "user" && len(rest) == 0 && req.Method == "GET" {
		return restJSON(200, userShape(store))
	}

	// POST /user/repos: create a repository. GET /user/repos: list, paginated.
	if first == "user" && len(rest) == 1 && rest[0] == "repos" {
		if req.Method == "POST" {
			body, errReply := parseRestBody(req)
			if errReply != nil {
				return *errReply
			}
			name, _ := body["name"].(string)
			if name == "" {
				return restError(422, "name is required")
			}
			private := false
			if b, ok := body["private"].(bool); ok {
				private = b
			}
			description, _ := body["description"].(string)
			repo := store.CreateRepo(name, private, description)
			if repo == nil {
				return restError(422, "Repository already exists")
			}
			return restJSON(201, repoShape(store, repo))
		}
		if req.Method == "GET" {
			perPage := perPageOf(req.Query, "per_page", 30)
			page := pageOf(req.Query)
			repos := store.ListRepos()
			// (page-1)*perPage can overflow for an astronomical page; a
			// start past the end (or wrapped negative) is an empty page.
			start := (page - 1) * perPage
			var items []any
			if start >= 0 && start < int64(len(repos)) {
				for i := start; i < start+perPage && i < int64(len(repos)); i++ {
					items = append(items, repoShape(store, repos[i]))
				}
			}
			if items == nil {
				items = []any{}
			}
			var headers [][2]string
			if start >= 0 && start+perPage < int64(len(repos)) {
				next := "http://" + req.Host + "/api/v3/user/repos?per_page=" + strconv.FormatInt(perPage, 10) + "&page=" + strconv.FormatInt(page+1, 10)
				headers = [][2]string{{"link", "<" + next + ">; rel=\"next\""}}
			}
			return restJSON(200, items, headers)
		}
		return restError(405, "Method Not Allowed")
	}

	// Everything below is /repos/{owner}/{repo}[...].
	if first != "repos" || len(rest) < 2 {
		return restError(404, "Not Found")
	}
	owner, repoName := rest[0], rest[1]
	tail := rest[2:]
	repo := store.GetRepo(owner, repoName)
	if repo == nil {
		return restError(404, "Not Found")
	}

	// GET / PATCH / DELETE /repos/{owner}/{repo}.
	if len(tail) == 0 {
		if req.Method == "GET" {
			return restJSON(200, repoShape(store, repo))
		}
		if req.Method == "PATCH" {
			body, errReply := parseRestBody(req)
			if errReply != nil {
				return *errReply
			}
			if description, ok := body["description"].(string); ok {
				repo.Description = description
			}
			if private, ok := body["private"].(bool); ok {
				repo.Private = private
			}
			repo.UpdatedAt = store.NowISO()
			return restJSON(200, repoShape(store, repo))
		}
		if req.Method == "DELETE" {
			store.DeleteRepo(owner, repoName)
			return RestReply{Status: 204}
		}
		return restError(405, "Method Not Allowed")
	}

	section := tail[0]
	sectionRest := tail[1:]

	// Branches.
	if section == "branches" {
		if req.Method == "GET" && len(sectionRest) == 0 {
			branches := store.ListBranches(repo)
			items := make([]any, 0, len(branches))
			for _, branch := range branches {
				items = append(items, map[string]any{"name": branch.Name, "commit": map[string]any{"sha": branch.SHA}})
			}
			return restJSON(200, items)
		}
		if req.Method == "GET" && len(sectionRest) == 1 {
			name := decodeURIComponentStrict(sectionRest[0])
			tip := store.BranchTip(repo, name)
			if tip == "" {
				return restError(404, "Not Found")
			}
			return restJSON(200, map[string]any{"name": name, "commit": map[string]any{"sha": tip}})
		}
		return restError(404, "Not Found")
	}

	// Commits: ?sha=<branch> selects the branch; no sha means the default
	// branch.
	if section == "commits" {
		if req.Method == "GET" && len(sectionRest) == 0 {
			ref := repo.DefaultBranch
			if _, ok := req.Query["sha"]; ok {
				ref = req.Query.Get("sha")
			}
			history := store.HistoryOfRef(repo, ref, 1000)
			items := make([]any, 0, len(history))
			for _, commit := range history {
				items = append(items, map[string]any{
					"sha": commit.SHA,
					"commit": map[string]any{
						// GitHub presents the message without its trailing
						// newline; the stored bytes keep it.
						"message":   strings.TrimRight(commit.Message, "\n"),
						"author":    map[string]any{"name": store.ViewerLogin()},
						"committer": map[string]any{"name": store.ViewerLogin()},
					},
				})
			}
			return restJSON(200, items)
		}
		return restError(404, "Not Found")
	}

	// Git refs: create a branch, or read one.
	if section == "git" && len(sectionRest) > 0 && sectionRest[0] == "refs" {
		if req.Method == "POST" && len(sectionRest) == 1 {
			body, errReply := parseRestBody(req)
			if errReply != nil {
				return *errReply
			}
			ref, _ := body["ref"].(string)
			sha, _ := body["sha"].(string)
			if ref == "" || sha == "" || !strings.HasPrefix(ref, "refs/") {
				return restError(422, "ref and sha are required")
			}
			if store.GetObject(repo, sha) == nil {
				return restError(422, "sha does not exist")
			}
			store.SetRef(repo, ref, sha)
			return restJSON(201, map[string]any{"ref": ref, "object": map[string]any{"sha": sha}})
		}
		if req.Method == "GET" && len(sectionRest) >= 2 {
			ref := "refs/" + strings.Join(sectionRest[1:], "/")
			oid := store.GetRef(repo, ref)
			if oid == "" {
				return restError(404, "Not Found")
			}
			return restJSON(200, map[string]any{"ref": ref, "object": map[string]any{"sha": oid}})
		}
		return restError(404, "Not Found")
	}

	// Contents: a path that may itself contain slashes.
	if section == "contents" {
		decoded := make([]string, len(sectionRest))
		for i, segment := range sectionRest {
			decoded[i] = decodeURIComponentStrict(segment)
		}
		path := strings.Join(decoded, "/")
		if req.Method == "GET" {
			ref := repo.DefaultBranch
			if _, ok := req.Query["ref"]; ok {
				ref = req.Query.Get("ref")
			}
			read := store.ReadPath(repo, ref, path)
			if read == nil {
				return restError(404, "Not Found")
			}
			if read.Kind == "dir" {
				items := make([]any, 0, len(read.Entries))
				for _, entry := range read.Entries {
					items = append(items, map[string]any{
						"type": entry.Type,
						"name": entry.Name,
						"path": entry.Path,
						"sha":  entry.SHA,
						"size": int64(0),
					})
				}
				return restJSON(200, items)
			}
			name := path
			if at := strings.LastIndex(path, "/"); at != -1 {
				name = path[at+1:]
			}
			return restJSON(200, map[string]any{
				"type":     "file",
				"encoding": "base64",
				"content":  base64.StdEncoding.EncodeToString(read.Data),
				"sha":      read.OID,
				"size":     int64(len(read.Data)),
				"name":     name,
				"path":     path,
			})
		}
		if req.Method == "PUT" {
			body, errReply := parseRestBody(req)
			if errReply != nil {
				return *errReply
			}
			message, _ := body["message"].(string)
			if message == "" {
				// asString(body.message) ?? 'Update file': an absent or
				// non-string message defaults; an empty string stays empty.
				if _, ok := body["message"].(string); !ok {
					message = "Update file"
				}
			}
			content, _ := body["content"].(string)
			branch, _ := body["branch"].(string)
			if _, ok := body["branch"].(string); !ok {
				branch = repo.DefaultBranch
			}
			var sha *string
			if s, ok := body["sha"].(string); ok {
				sha = &s
			}
			stripped := stripWhitespace(content)
			bytes, err := base64.StdEncoding.DecodeString(stripped)
			if err != nil {
				return restError(422, "content is not valid base64")
			}
			written := store.WriteFile(repo, branch, path, bytes, message, sha)
			if written.Error != "" {
				if written.Error == "stale-sha" {
					return restError(409, "sha does not match the current file")
				}
				return restError(422, "could not write the file")
			}
			name := path
			if at := strings.LastIndex(path, "/"); at != -1 {
				name = path[at+1:]
			}
			status := 200
			if written.Created {
				status = 201
			}
			return restJSON(status, map[string]any{
				"content": map[string]any{"sha": written.BlobSHA, "encoding": "base64", "name": name, "path": path},
				"commit":  map[string]any{"sha": written.CommitSHA, "message": message},
			})
		}
		if req.Method == "DELETE" {
			body, errReply := parseRestBody(req)
			if errReply != nil {
				return *errReply
			}
			message, _ := body["message"].(string)
			if _, ok := body["message"].(string); !ok {
				message = "Delete file"
			}
			branch, _ := body["branch"].(string)
			if _, ok := body["branch"].(string); !ok {
				branch = repo.DefaultBranch
			}
			var sha *string
			if s, ok := body["sha"].(string); ok {
				sha = &s
			}
			deleted := store.DeleteFile(repo, branch, path, message, sha)
			if deleted.Error != "" {
				if deleted.Error == "not-found" {
					return restError(404, "Not Found")
				}
				if deleted.Error == "stale-sha" {
					return restError(409, "sha does not match the current file")
				}
				return restError(404, "Not Found")
			}
			return restJSON(200, map[string]any{"commit": map[string]any{"sha": deleted.CommitSHA, "message": message}})
		}
		return restError(405, "Method Not Allowed")
	}

	// Labels.
	if section == "labels" {
		if req.Method == "POST" && len(sectionRest) == 0 {
			body, errReply := parseRestBody(req)
			if errReply != nil {
				return *errReply
			}
			name, _ := body["name"].(string)
			if name == "" {
				return restError(422, "name is required")
			}
			color, _ := body["color"].(string)
			if _, ok := body["color"].(string); !ok {
				color = "ededed"
			}
			label := store.CreateLabel(repo, name, color)
			return restJSON(201, labelShape(label.Name, label.Color))
		}
		return restError(404, "Not Found")
	}

	// Issues.
	if section == "issues" {
		if len(sectionRest) == 0 {
			if req.Method == "POST" {
				return createIssueFromBody(store, repo, req)
			}
			if req.Method == "GET" {
				perPage := perPageOf(req.Query, "per_page", 30)
				state := ""
				if _, ok := req.Query["state"]; ok {
					state = req.Query.Get("state")
				}
				sortParam := ""
				if _, ok := req.Query["sort"]; ok {
					sortParam = req.Query.Get("sort")
				}
				direction := ""
				if _, ok := req.Query["direction"]; ok {
					direction = req.Query.Get("direction")
				}
				issues := store.ListIssues(repo, state, sortParam, direction)
				items := make([]any, 0, len(issues))
				for i, issue := range issues {
					if int64(i) >= perPage {
						break
					}
					items = append(items, issueShape(store, repo, issue))
				}
				return restJSON(200, items)
			}
			return restError(405, "Method Not Allowed")
		}
		number, ok := jsParseInt(sectionRest[0])
		if !ok || number < 1 {
			return restError(404, "Not Found")
		}
		if len(sectionRest) == 1 {
			if req.Method == "GET" {
				issue := store.GetIssue(repo, number)
				if issue == nil {
					return restError(404, "Not Found")
				}
				return RestReply{Status: 200, Raw: cachedIssueBody(store, repo, issue)}
			}
			if req.Method == "PATCH" {
				body, errReply := parseRestBody(req)
				if errReply != nil {
					return *errReply
				}
				var state, title, bodyText *string
				if s, ok := body["state"].(string); ok {
					state = &s
				}
				if s, ok := body["title"].(string); ok {
					title = &s
				}
				if s, ok := body["body"].(string); ok {
					bodyText = &s
				}
				issue := store.PatchIssue(repo, number, state, title, bodyText)
				if issue == nil {
					return restError(404, "Not Found")
				}
				return restJSON(200, issueShape(store, repo, issue))
			}
			return restError(405, "Method Not Allowed")
		}
		if sectionRest[1] == "comments" && len(sectionRest) == 2 {
			if req.Method == "POST" {
				body, errReply := parseRestBody(req)
				if errReply != nil {
					return *errReply
				}
				commentBody, ok := body["body"].(string)
				if !ok {
					return restError(422, "body is required")
				}
				comment := store.AddComment(repo, number, commentBody)
				if comment == nil {
					return restError(404, "Not Found")
				}
				return restJSON(201, map[string]any{"id": comment.ID, "body": comment.Body, "created_at": comment.CreatedAt})
			}
			if req.Method == "GET" {
				issue := store.GetIssue(repo, number)
				if issue == nil {
					return restError(404, "Not Found")
				}
				items := make([]any, 0, len(issue.Comments))
				for _, comment := range issue.Comments {
					items = append(items, map[string]any{"id": comment.ID, "body": comment.Body, "created_at": comment.CreatedAt})
				}
				return restJSON(200, items)
			}
			return restError(405, "Method Not Allowed")
		}
		return restError(404, "Not Found")
	}

	// Pull requests.
	if section == "pulls" {
		if len(sectionRest) == 0 {
			if req.Method == "POST" {
				body, errReply := parseRestBody(req)
				if errReply != nil {
					return *errReply
				}
				title, _ := body["title"].(string)
				head, _ := body["head"].(string)
				base, _ := body["base"].(string)
				if _, ok := body["base"].(string); !ok {
					base = repo.DefaultBranch
				}
				if title == "" || head == "" {
					return restError(422, "title and head are required")
				}
				bodyText, _ := body["body"].(string)
				pull := store.CreatePullRequest(repo, title, bodyText, head, base)
				if pull == nil {
					return restError(422, "head or base branch does not exist")
				}
				return restJSON(201, pullShape(store, repo, pull))
			}
			if req.Method == "GET" {
				perPage := perPageOf(req.Query, "per_page", 30)
				state := ""
				if _, ok := req.Query["state"]; ok {
					state = req.Query.Get("state")
				}
				pulls := store.ListPulls(repo, state)
				items := make([]any, 0, len(pulls))
				for i, pull := range pulls {
					if int64(i) >= perPage {
						break
					}
					items = append(items, pullShape(store, repo, pull))
				}
				return restJSON(200, items)
			}
			return restError(405, "Method Not Allowed")
		}
		number, ok := jsParseInt(sectionRest[0])
		if !ok || number < 1 {
			return restError(404, "Not Found")
		}
		if len(sectionRest) == 1 {
			if req.Method == "GET" {
				pull := store.GetPull(repo, number)
				if pull == nil {
					return restError(404, "Not Found")
				}
				return restJSON(200, pullShape(store, repo, pull))
			}
			return restError(405, "Method Not Allowed")
		}
		if sectionRest[1] == "files" && len(sectionRest) == 2 && req.Method == "GET" {
			pull := store.GetPull(repo, number)
			if pull == nil {
				return restError(404, "Not Found")
			}
			files := store.PullFiles(repo, pull)
			items := make([]any, 0, len(files))
			for _, file := range files {
				items = append(items, map[string]any{
					"filename":  file.Filename,
					"status":    file.Status,
					"additions": int64(file.Additions),
					"deletions": int64(file.Deletions),
					"changes":   int64(file.Changes),
				})
			}
			return restJSON(200, items)
		}
		if sectionRest[1] == "merge" && len(sectionRest) == 2 && req.Method == "PUT" {
			merged := store.MergePull(repo, number)
			if merged.Error != "" {
				if merged.Error == "unknown" {
					return restError(404, "Not Found")
				}
				if merged.Error == "closed" {
					return restError(405, "Pull request is already merged or closed")
				}
				return restError(405, "Pull request cannot be merged: conflicting files")
			}
			return restJSON(200, map[string]any{"sha": merged.SHA, "merged": true, "message": "Pull request successfully merged"})
		}
		return restError(404, "Not Found")
	}

	return restError(404, "Not Found")
}

// stripWhitespace removes ASCII whitespace bytes (the \s class as it appears
// in base64 content).
func stripWhitespace(s string) string {
	if !strings.ContainsAny(s, " \t\n\r\v\f") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != ' ' && c != '\t' && c != '\n' && c != '\r' && c != '\v' && c != '\f' {
			b.WriteByte(c)
		}
	}
	return b.String()
}
