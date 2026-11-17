// GraphQL v4: a small executor for the fixed documents the suite sends (the
// documents `gh` and github-mcp-server send). It parses queries and mutations,
// validates field names against the schema, and resolves them against the same
// store REST uses, so an issue created over GraphQL reads back over REST with
// the same number, title and state (GraphQL OPEN/CLOSED, REST open/closed).
//
// Wire contract: POST /api/graphql, one operation {query, variables} or a
// batch (a JSON array of them, answered as an array in order). A GraphQL-level
// failure is HTTP 200 with an `errors` array, never an HTTP error. An unknown
// repository is data.repository = null with no errors. No Authorization header
// is exactly 401 (enforced by the router).

package main

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// --------------------------------------------------------------------------- lexer

type tokenKind int

const (
	tokName tokenKind = iota
	tokString
	tokInt
	tokFloat
	tokPunct
	tokEOF
)

type gqlToken struct {
	kind       tokenKind
	str        string
	intVal     int64
	floatVal   float64
}

func lexGQL(source string) ([]gqlToken, error) {
	var tokens []gqlToken
	i := 0
	isNameStart := func(ch byte) bool {
		return ch == '_' || (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z')
	}
	isNameChar := func(ch byte) bool {
		return isNameStart(ch) || (ch >= '0' && ch <= '9')
	}
	isDigit := func(ch byte) bool { return ch >= '0' && ch <= '9' }
	for i < len(source) {
		ch := source[i]
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',' {
			i++
			continue
		}
		if ch == '#' {
			for i < len(source) && source[i] != '\n' {
				i++
			}
			continue
		}
		if ch == '.' && i+3 <= len(source) && source[i:i+3] == "..." {
			tokens = append(tokens, gqlToken{kind: tokPunct, str: "..."})
			i += 3
			continue
		}
		if strings.IndexByte("{}()[]:!=$@|&=", ch) != -1 {
			tokens = append(tokens, gqlToken{kind: tokPunct, str: string(ch)})
			i++
			continue
		}
		if ch == '"' {
			// A string literal (block strings are not used by the suite's
			// documents).
			var value strings.Builder
			i++
			for i < len(source) && source[i] != '"' {
				if source[i] == '\\' && i+1 < len(source) {
					escaped := source[i+1]
					switch escaped {
					case 'n':
						value.WriteByte('\n')
					case 't':
						value.WriteByte('\t')
					case 'r':
						value.WriteByte('\r')
					default:
						value.WriteByte(escaped)
					}
					i += 2
				} else {
					value.WriteByte(source[i])
					i++
				}
			}
			i++ // the closing quote
			tokens = append(tokens, gqlToken{kind: tokString, str: value.String()})
			continue
		}
		if ch == '-' || isDigit(ch) {
			start := i
			for i < len(source) && (isDigit(source[i]) || source[i] == 'e' || source[i] == 'E' || source[i] == '.' || source[i] == '+' || source[i] == '-') {
				i++
			}
			text := source[start:i]
			if strings.ContainsAny(text, ".eE") {
				f, _ := jsParseFloat(text)
				tokens = append(tokens, gqlToken{kind: tokFloat, floatVal: f})
			} else {
				n, _ := jsParseInt(text)
				tokens = append(tokens, gqlToken{kind: tokInt, intVal: n})
			}
			continue
		}
		if isNameStart(ch) {
			start := i
			for i < len(source) && isNameChar(source[i]) {
				i++
			}
			tokens = append(tokens, gqlToken{kind: tokName, str: source[start:i]})
			continue
		}
		return nil, fmt.Errorf("graphql: unexpected character %q", string(ch))
	}
	tokens = append(tokens, gqlToken{kind: tokEOF})
	return tokens, nil
}

// --------------------------------------------------------------------------- parser

// gqlEnum is an enum literal in an argument value.
type gqlEnum struct{ value string }

// gqlVariable is a variable reference in an argument value.
type gqlVariable struct{ name string }

// fieldNode is one field selection: [alias :] name [(args)] [selection set].
type fieldNode struct {
	alias      string // "" when none
	name       string
	args       map[string]any
	selections []selectionNode
}

// fragmentNode is one inline fragment: ... on Type { selections }.
type fragmentNode struct {
	typeCondition string
	selections    []selectionNode
}

// selectionNode is a field or an inline fragment.
type selectionNode interface{ isSelection() }

func (*fieldNode) isSelection()    {}
func (*fragmentNode) isSelection() {}

// operationNode is one query or mutation operation.
type operationNode struct {
	kind       string // "query" | "mutation"
	name       string // "" when anonymous
	selections []selectionNode
}

type gqlParser struct {
	tokens []gqlToken
	at     int
}

func (p *gqlParser) peek() gqlToken { return p.tokens[p.at] }

func (p *gqlParser) next() gqlToken {
	tok := p.tokens[p.at]
	if tok.kind != tokEOF {
		p.at++
	}
	return tok
}

func (p *gqlParser) expectPunct(value string) error {
	tok := p.next()
	if tok.kind != tokPunct || tok.str != value {
		return fmt.Errorf("graphql: expected %q", value)
	}
	return nil
}

func (p *gqlParser) expectName() (string, error) {
	tok := p.next()
	if tok.kind != tokName {
		return "", fmt.Errorf("graphql: expected a name")
	}
	return tok.str, nil
}

func (p *gqlParser) parseDocument() ([]*operationNode, error) {
	var operations []*operationNode
	for p.peek().kind != tokEOF {
		op, err := p.parseOperation()
		if err != nil {
			return nil, err
		}
		operations = append(operations, op)
	}
	return operations, nil
}

func (p *gqlParser) parseOperation() (*operationNode, error) {
	kind := "query"
	name := ""
	tok := p.peek()
	if tok.kind == tokName && (tok.str == "query" || tok.str == "mutation") {
		kind = tok.str
		p.next()
		if maybeName := p.peek(); maybeName.kind == tokName {
			name = p.next().str
		}
		// Variable definitions: ($name: Type, ...) — the types are not
		// needed to execute.
		if peek := p.peek(); peek.kind == tokPunct && peek.str == "(" {
			if err := p.skipParens(); err != nil {
				return nil, err
			}
		}
	}
	selections, err := p.parseSelectionSet()
	if err != nil {
		return nil, err
	}
	return &operationNode{kind: kind, name: name, selections: selections}, nil
}

// skipParens skips a balanced (...) group (variable definitions and their
// defaults).
func (p *gqlParser) skipParens() error {
	depth := 0
	for {
		tok := p.next()
		if tok.kind == tokEOF {
			return fmt.Errorf("graphql: unbalanced parentheses")
		}
		if tok.kind == tokPunct && tok.str == "(" {
			depth++
		}
		if tok.kind == tokPunct && tok.str == ")" {
			depth--
			if depth == 0 {
				return nil
			}
		}
	}
}

func (p *gqlParser) parseSelectionSet() ([]selectionNode, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var selections []selectionNode
	for {
		tok := p.peek()
		if tok.kind == tokPunct && tok.str == "}" {
			p.next()
			return selections, nil
		}
		if tok.kind == tokEOF {
			return nil, fmt.Errorf("graphql: unterminated selection set")
		}
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		selections = append(selections, sel)
	}
}

func (p *gqlParser) parseSelection() (selectionNode, error) {
	tok := p.peek()
	if tok.kind == tokPunct && tok.str == "..." {
		p.next()
		on := p.next()
		if on.kind != tokName || on.str != "on" {
			return nil, fmt.Errorf("graphql: only inline fragments are supported")
		}
		typeCondition, err := p.expectName()
		if err != nil {
			return nil, err
		}
		selections, err := p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
		return &fragmentNode{typeCondition: typeCondition, selections: selections}, nil
	}
	// A field: [alias :] name [(args)] [selection set].
	name, err := p.expectName()
	if err != nil {
		return nil, err
	}
	alias := ""
	if peek := p.peek(); peek.kind == tokPunct && peek.str == ":" {
		p.next()
		alias = name
		name, err = p.expectName()
		if err != nil {
			return nil, err
		}
	}
	args := map[string]any{}
	if peek := p.peek(); peek.kind == tokPunct && peek.str == "(" {
		args, err = p.parseArguments()
		if err != nil {
			return nil, err
		}
	}
	var selections []selectionNode
	if peek := p.peek(); peek.kind == tokPunct && peek.str == "{" {
		selections, err = p.parseSelectionSet()
		if err != nil {
			return nil, err
		}
	}
	return &fieldNode{alias: alias, name: name, args: args, selections: selections}, nil
}

func (p *gqlParser) parseArguments() (map[string]any, error) {
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	args := map[string]any{}
	for {
		tok := p.peek()
		if tok.kind == tokPunct && tok.str == ")" {
			p.next()
			return args, nil
		}
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		value, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args[name] = value
	}
}

func (p *gqlParser) parseValue() (any, error) {
	tok := p.next()
	switch tok.kind {
	case tokString:
		return tok.str, nil
	case tokInt:
		return tok.intVal, nil
	case tokFloat:
		return tok.floatVal, nil
	case tokName:
		switch tok.str {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "null":
			return nil, nil
		default:
			return gqlEnum{value: tok.str}, nil
		}
	}
	if tok.kind == tokPunct && tok.str == "$" {
		name, err := p.expectName()
		if err != nil {
			return nil, err
		}
		return gqlVariable{name: name}, nil
	}
	if tok.kind == tokPunct && tok.str == "[" {
		var values []any
		for {
			if peek := p.peek(); peek.kind == tokPunct && peek.str == "]" {
				break
			}
			value, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		p.next()
		return values, nil
	}
	if tok.kind == tokPunct && tok.str == "{" {
		object := map[string]any{}
		for {
			if peek := p.peek(); peek.kind == tokPunct && peek.str == "}" {
				break
			}
			name, err := p.expectName()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			value, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			object[name] = value
		}
		p.next()
		return object, nil
	}
	return nil, fmt.Errorf("graphql: unexpected value")
}

// --------------------------------------------------------------------------- values

// gqlObject is a resolved object: its GraphQL type and the store object or
// data behind it.
type gqlObject struct {
	typename string
	value    any
}

func gqlObj(typename string, value any) *gqlObject {
	return &gqlObject{typename: typename, value: value}
}

// resolveGQLValue substitutes variables in an argument value; enums become
// their plain string.
func resolveGQLValue(value any, variables map[string]any) any {
	switch t := value.(type) {
	case gqlVariable:
		if v, ok := variables[t.name]; ok {
			return v
		}
		return nil
	case gqlEnum:
		return t.value
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = resolveGQLValue(item, variables)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, item := range t {
			out[k] = resolveGQLValue(item, variables)
		}
		return out
	default:
		return value
	}
}

func argsOf(field *fieldNode, variables map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range field.args {
		out[k] = resolveGQLValue(v, variables)
	}
	return out
}

// --------------------------------------------------------------------------- global ids

// GlobalID builds an opaque, stable global id: base64url("<Type>:<key>").
// Never the bare numeric id.
func GlobalID(typ, key string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(typ + ":" + key))
}

type parsedGlobalID struct {
	typ string
	key string
}

func parseGlobalID(id any) *parsedGlobalID {
	s, ok := id.(string)
	if !ok {
		return nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		// Node's Buffer.from(id, 'base64url') is lenient; a strict failure
		// here still ends at null, the same as an undecodable id there.
		return nil
	}
	at := strings.Index(string(decoded), ":")
	if at == -1 {
		return nil
	}
	return &parsedGlobalID{typ: string(decoded)[:at], key: string(decoded)[at+1:]}
}

// --------------------------------------------------------------------------- schema

// gqlFields is the fields each type answers (validation: an unknown field is
// an error naming it).
var gqlFields = map[string]map[string]bool{
	"Query":    {"viewer": true, "repository": true, "node": true, "__type": true},
	"Mutation": {"createIssue": true, "closeIssue": true, "createPullRequest": true, "mergePullRequest": true, "addComment": true},
	"User":     {"login": true, "id": true},
	"Repository": {"id": true, "databaseId": true, "name": true, "nameWithOwner": true, "owner": true,
		"defaultBranchRef": true, "viewerPermission": true, "hasIssuesEnabled": true, "issue": true,
		"issues": true, "pullRequests": true, "pullRequest": true},
	"Ref":                  {"name": true},
	"Issue":                {"id": true, "number": true, "title": true, "state": true, "body": true},
	"IssueConnection":      {"nodes": true, "pageInfo": true, "totalCount": true},
	"PageInfo":             {"hasNextPage": true, "endCursor": true, "hasPreviousPage": true, "startCursor": true},
	"PullRequest": {"id": true, "number": true, "title": true, "state": true, "merged": true,
		"headRefName": true, "baseRefName": true, "mergeable": true, "files": true},
	"PullRequestConnection":  {"nodes": true, "pageInfo": true, "totalCount": true},
	"PullRequestChangedFile": {"path": true},
	"IssueComment":           {"id": true, "body": true},
	"CommentEdge":            {"node": true},
	"CreateIssuePayload":     {"issue": true},
	"CloseIssuePayload":      {"issue": true},
	"CreatePullRequestPayload": {"pullRequest": true},
	"MergePullRequestPayload":  {"pullRequest": true},
	"AddCommentPayload":        {"commentEdge": true},
	"__Type":                   {"name": true, "fields": true, "enumValues": true},
	"__Field":                  {"name": true},
	"__EnumValue":              {"name": true},
}

// typeInfo is the introspection data: field and enum names per type (extra
// names are harmless).
type typeInfo struct {
	fields     []string
	enumValues []string
}

var gqlTypeInfo = map[string]typeInfo{
	"Issue": {
		fields:     []string{"id", "number", "title", "state", "body", "createdAt", "updatedAt", "closed", "author", "comments", "labels"},
		enumValues: []string{},
	},
	"PullRequest": {
		fields:     []string{"id", "number", "title", "state", "merged", "headRefName", "baseRefName", "mergeable", "files", "body", "createdAt", "updatedAt", "author"},
		enumValues: []string{},
	},
	"Repository": {
		fields:     []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "isPrivate", "createdAt", "updatedAt"},
		enumValues: []string{},
	},
	"SearchType":     {fields: []string{}, enumValues: []string{"ISSUE", "REPOSITORY", "USER"}},
	"User":           {fields: []string{"login", "id", "name"}, enumValues: []string{}},
	"IssueState":     {fields: []string{}, enumValues: []string{"OPEN", "CLOSED"}},
	"MergeableState": {fields: []string{}, enumValues: []string{"MERGEABLE", "CONFLICTING", "UNKNOWN"}},
	"PullRequestState": {fields: []string{}, enumValues: []string{"OPEN", "CLOSED", "MERGED"}},
}

// cursorOf returns the cursor of an issue in a connection:
// base64url("cursor:<createdSeq>").
func cursorOf(issue *Issue) string {
	return base64.RawURLEncoding.EncodeToString([]byte("cursor:" + strconv.FormatInt(issue.CreatedSeq, 10)))
}

func seqOfCursor(cursor any) (int64, bool) {
	s, ok := cursor.(string)
	if !ok {
		return 0, false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return 0, false
	}
	if !strings.HasPrefix(string(decoded), "cursor:") {
		return 0, false
	}
	seq, ok := jsParseInt(string(decoded)[len("cursor:"):])
	return seq, ok
}

// jsSliceEnd computes the end index of a JS Array.slice(0, end) over a slice
// of the given length: a negative end counts from the end, out-of-range ends
// clamp.
func jsSliceEnd(length, end int64) int64 {
	if end < 0 {
		end = length + end
	}
	if end < 0 {
		end = 0
	}
	if end > length {
		end = length
	}
	return end
}

// jsNumberToInt converts a value the way JS Number() followed by an integer
// comparison would: it returns the int when Number(v) is an integer, else
// false. Strings parse as whole numbers (Number("5") is 5, Number("5x") is
// NaN, Number("") is 0); booleans are 1/0; null is NaN.
func jsNumberToInt(v any) (int64, bool) {
	switch t := v.(type) {
	case int64:
		return t, true
	case int:
		return int64(t), true
	case float64:
		if t != float64(int64(t)) {
			// NaN, infinities and fractions never match an integer id.
			return 0, false
		}
		return int64(t), true
	case string:
		trimmed := strings.Trim(t, " \t\n\r\v\f")
		if trimmed == "" {
			return 0, true
		}
		f, ok := jsParseFloat(trimmed)
		if !ok {
			return 0, false
		}
		// Number() requires the WHOLE string to be a number.
		rest := trimmed
		// Re-scan: jsParseFloat takes the longest prefix; verify it consumed
		// everything (modulo the sign/Infinity forms it handles).
		if !jsFloatConsumesAll(rest, f) {
			return 0, false
		}
		if f != float64(int64(f)) {
			return 0, false
		}
		return int64(f), true
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	default:
		return 0, false
	}
}

// jsFloatConsumesAll reports whether the whole string is one float literal
// (the value jsParseFloat found), i.e. JS Number() would accept it.
func jsFloatConsumesAll(s string, f float64) bool {
	// Recompute the consumed prefix length by re-scanning with the same rules
	// jsParseFloat uses.
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	if strings.HasPrefix(s[i:], "Infinity") {
		return i+len("Infinity") == len(s)
	}
	digits := false
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
		digits = true
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			i++
			digits = true
		}
	}
	if !digits {
		return false
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < len(s) && (s[j] == '+' || s[j] == '-') {
			j++
		}
		if j < len(s) && s[j] >= '0' && s[j] <= '9' {
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			i = j
		}
	}
	return i == len(s)
}

// --------------------------------------------------------------------------- executor

type gqlExecutor struct {
	errors []map[string]any
	store  *Store
}

func (e *gqlExecutor) addError(message string) {
	e.errors = append(e.errors, map[string]any{"message": message})
}

// execute runs one operation's selection set against the Query/Mutation root.
func (e *gqlExecutor) execute(operation *operationNode, variables map[string]any) map[string]any {
	rootType := "Query"
	if operation.kind == "mutation" {
		rootType = "Mutation"
	}
	root := gqlObj(rootType, nil)
	return e.executeSelections(root, operation.selections, variables)
}

func (e *gqlExecutor) executeSelections(parent *gqlObject, selections []selectionNode, variables map[string]any) map[string]any {
	out := map[string]any{}
	for _, selection := range selections {
		switch sel := selection.(type) {
		case *fragmentNode:
			if sel.typeCondition != parent.typename {
				continue
			}
			merged := e.executeSelections(parent, sel.selections, variables)
			for k, v := range merged {
				out[k] = v
			}
		case *fieldNode:
			key := sel.name
			if sel.alias != "" {
				key = sel.alias
			}
			out[key] = e.executeField(parent, sel, variables)
		}
	}
	return out
}

func (e *gqlExecutor) executeField(parent *gqlObject, field *fieldNode, variables map[string]any) any {
	allowed := gqlFields[parent.typename]
	if field.name == "__typename" {
		return parent.typename
	}
	if allowed == nil || !allowed[field.name] {
		e.addError(fmt.Sprintf("Cannot query field %q on type %q.", field.name, parent.typename))
		return nil
	}
	args := argsOf(field, variables)
	resolved := e.resolve(parent, field.name, args)
	if resolved == nil {
		return nil
	}
	switch t := resolved.(type) {
	case []*gqlObject:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = e.complete(item, field.selections, variables)
		}
		return out
	case *gqlObject:
		return e.complete(t, field.selections, variables)
	default:
		return resolved
	}
}

// complete resolves a value: scalars pass through, objects resolve their
// sub-selections.
func (e *gqlExecutor) complete(value any, selections []selectionNode, variables map[string]any) any {
	if value == nil {
		return nil
	}
	switch t := value.(type) {
	case []*gqlObject:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = e.complete(item, selections, variables)
		}
		return out
	case *gqlObject:
		if len(selections) == 0 {
			return map[string]any{}
		}
		return e.executeSelections(t, selections, variables)
	default:
		return value
	}
}

// refValue is the value behind a Ref object.
type refValue struct{ name string }

// issueConnectionValue is the value behind an IssueConnection object.
type issueConnectionValue struct {
	nodes    []*gqlObject
	pageInfo *gqlObject
}

// pageInfoValue is the value behind a PageInfo object.
type pageInfoValue struct {
	hasNextPage bool
	endCursor   any // string or nil
}

// pullConnectionValue is the value behind a PullRequestConnection object.
type pullConnectionValue struct {
	nodes []*gqlObject
}

// changedFileValue is the value behind a PullRequestChangedFile object.
type changedFileValue struct{ path string }

// commentValue is the value behind an IssueComment object.
type commentValue struct {
	id     int64
	body   string
	repoID int64
}

// pullRefValue is the value behind a PullRequest object.
type pullRefValue struct {
	repo *Repo
	pull *PullRequest
}

// payloadValue is the value behind a mutation payload object.
type payloadValue struct {
	issue       *gqlObject
	pullRequest *gqlObject
	commentEdge *gqlObject
}

// typeValue is the value behind a __Type object.
type typeValue struct {
	name       string
	info       typeInfo
}

// namedValue is the value behind a __Field / __EnumValue object.
type namedValue struct{ name string }

// resolve is the field resolver, by parent type.
func (e *gqlExecutor) resolve(parent *gqlObject, field string, args map[string]any) any {
	store := e.store
	switch parent.typename {
	case "Query":
		switch field {
		case "viewer":
			return gqlObj("User", nil)
		case "repository":
			repo := store.GetRepo(jsString(args["owner"]), jsString(args["name"]))
			if repo == nil {
				return nil
			}
			return gqlObj("Repository", repo)
		case "node":
			parsed := parseGlobalID(args["id"])
			if parsed == nil || parsed.typ != "Repository" {
				return nil
			}
			id, ok := jsParseInt(parsed.key)
			if !ok {
				return nil
			}
			for _, repo := range store.ListRepos() {
				if repo.ID == id {
					return gqlObj("Repository", repo)
				}
			}
			return nil
		case "__type":
			info, ok := gqlTypeInfo[jsString(args["name"])]
			if !ok {
				return nil
			}
			return gqlObj("__Type", typeValue{name: jsString(args["name"]), info: info})
		}
		return nil
	case "Mutation":
		switch field {
		case "createIssue":
			return e.mutateCreateIssue(args)
		case "closeIssue":
			return e.mutateCloseIssue(args)
		case "createPullRequest":
			return e.mutateCreatePullRequest(args)
		case "mergePullRequest":
			return e.mutateMergePullRequest(args)
		case "addComment":
			return e.mutateAddComment(args)
		}
		return nil
	case "User":
		switch field {
		case "login":
			return store.ViewerLogin()
		case "id":
			return GlobalID("User", "1000")
		}
		return nil
	case "Repository":
		repo := parent.value.(*Repo)
		switch field {
		case "id":
			return GlobalID("Repository", strconv.FormatInt(repo.ID, 10))
		case "databaseId":
			return repo.ID
		case "name":
			return repo.Name
		case "nameWithOwner":
			return store.ViewerLogin() + "/" + repo.Name
		case "owner":
			return gqlObj("User", nil)
		case "defaultBranchRef":
			return gqlObj("Ref", refValue{name: repo.DefaultBranch})
		case "viewerPermission":
			return "ADMIN"
		case "hasIssuesEnabled":
			return true
		case "issue":
			number, ok := jsNumberToInt(args["number"])
			if !ok {
				return nil
			}
			issue := store.GetIssue(repo, number)
			if issue == nil {
				return nil
			}
			return gqlObj("Issue", issue)
		case "issues":
			return e.issuesConnection(repo, args)
		case "pullRequests":
			pulls := store.ListPulls(repo, "all")
			first := int64(len(pulls))
			if f, ok := toIntTrunc(args["first"]); ok {
				first = f
			}
			end := jsSliceEnd(int64(len(pulls)), first)
			nodes := make([]*gqlObject, 0, end)
			for i := int64(0); i < end; i++ {
				nodes = append(nodes, gqlObj("PullRequest", pullRefValue{repo: repo, pull: pulls[i]}))
			}
			return gqlObj("PullRequestConnection", pullConnectionValue{nodes: nodes})
		case "pullRequest":
			number, ok := jsNumberToInt(args["number"])
			if !ok {
				return nil
			}
			pull := store.GetPull(repo, number)
			if pull == nil {
				return nil
			}
			return gqlObj("PullRequest", pullRefValue{repo: repo, pull: pull})
		}
		return nil
	case "Ref":
		return parent.value.(refValue).name
	case "Issue":
		issue := parent.value.(*Issue)
		switch field {
		case "id":
			return GlobalID("Issue", strconv.FormatInt(issue.ID, 10))
		case "number":
			return issue.Number
		case "title":
			return issue.Title
		case "state":
			if issue.State == "open" {
				return "OPEN"
			}
			return "CLOSED"
		case "body":
			return issue.Body
		}
		return nil
	case "IssueConnection":
		connection := parent.value.(issueConnectionValue)
		switch field {
		case "nodes":
			return connection.nodes
		case "pageInfo":
			return connection.pageInfo
		case "totalCount":
			return int64(len(connection.nodes))
		}
		return nil
	case "PageInfo":
		info := parent.value.(pageInfoValue)
		switch field {
		case "hasNextPage":
			return info.hasNextPage
		case "endCursor":
			return info.endCursor
		case "hasPreviousPage":
			return false
		case "startCursor":
			return nil
		}
		return nil
	case "PullRequest":
		ref := parent.value.(pullRefValue)
		repo, pull := ref.repo, ref.pull
		switch field {
		case "id":
			return GlobalID("PullRequest", strconv.FormatInt(pull.ID, 10))
		case "number":
			return pull.Number
		case "title":
			return pull.Title
		case "state":
			if pull.Merged {
				return "MERGED"
			}
			if pull.State == "open" {
				return "OPEN"
			}
			return "CLOSED"
		case "merged":
			return pull.Merged
		case "headRefName":
			return pull.HeadRef
		case "baseRefName":
			return pull.BaseRef
		case "mergeable":
			if store.PullMergeable(repo, pull) {
				return "MERGEABLE"
			}
			return "CONFLICTING"
		case "files":
			first := int64(10)
			if f, ok := toIntTrunc(args["first"]); ok {
				first = f
			}
			paths := store.PullFilePaths(repo, pull)
			end := jsSliceEnd(int64(len(paths)), first)
			nodes := make([]*gqlObject, 0, end)
			for i := int64(0); i < end; i++ {
				nodes = append(nodes, gqlObj("PullRequestChangedFile", changedFileValue{path: paths[i]}))
			}
			return gqlObj("PullRequestConnection", pullConnectionValue{nodes: nodes})
		}
		return nil
	case "PullRequestConnection":
		connection := parent.value.(pullConnectionValue)
		switch field {
		case "nodes":
			return connection.nodes
		case "pageInfo":
			return gqlObj("PageInfo", pageInfoValue{hasNextPage: false, endCursor: nil})
		case "totalCount":
			return int64(len(connection.nodes))
		}
		return nil
	case "PullRequestChangedFile":
		return parent.value.(changedFileValue).path
	case "IssueComment":
		comment := parent.value.(commentValue)
		switch field {
		case "id":
			return GlobalID("IssueComment", strconv.FormatInt(comment.repoID, 10)+":"+strconv.FormatInt(comment.id, 10))
		case "body":
			return comment.body
		}
		return nil
	case "CommentEdge":
		if field == "node" {
			// A nil node must come back as an untyped nil: a typed nil
			// *gqlObject inside an any is non-nil and would panic in
			// complete().
			node := parent.value.(payloadValue).commentEdge
			if node == nil {
				return nil
			}
			return node
		}
		return nil
	case "CreateIssuePayload", "CloseIssuePayload":
		if field == "issue" {
			issue := parent.value.(payloadValue).issue
			if issue == nil {
				return nil
			}
			return issue
		}
		return nil
	case "CreatePullRequestPayload", "MergePullRequestPayload":
		if field == "pullRequest" {
			pull := parent.value.(payloadValue).pullRequest
			if pull == nil {
				return nil
			}
			return pull
		}
		return nil
	case "AddCommentPayload":
		if field == "commentEdge" {
			return parent.value.(payloadValue).commentEdge
		}
		return nil
	case "__Type":
		info := parent.value.(typeValue)
		switch field {
		case "name":
			return info.name
		case "fields":
			nodes := make([]*gqlObject, 0, len(info.info.fields))
			for _, name := range info.info.fields {
				nodes = append(nodes, gqlObj("__Field", namedValue{name: name}))
			}
			return nodes
		case "enumValues":
			nodes := make([]*gqlObject, 0, len(info.info.enumValues))
			for _, name := range info.info.enumValues {
				nodes = append(nodes, gqlObj("__EnumValue", namedValue{name: name}))
			}
			return nodes
		}
		return nil
	case "__Field", "__EnumValue":
		return parent.value.(namedValue).name
	}
	return nil
}

// issuesConnection builds the issues connection: states filter, orderBy,
// first/after pagination.
func (e *gqlExecutor) issuesConnection(repo *Repo, args map[string]any) *gqlObject {
	issues := append([]*Issue{}, repo.Issues...)
	if states, ok := args["states"].([]any); ok && len(states) > 0 {
		wanted := map[string]bool{}
		for _, state := range states {
			wanted[strings.ToLower(jsString(state))] = true
		}
		filtered := issues[:0]
		for _, issue := range issues {
			if wanted[issue.State] {
				filtered = append(filtered, issue)
			}
		}
		issues = filtered
	}
	direction := int64(-1)
	if orderBy, ok := args["orderBy"].(map[string]any); ok {
		if dir, ok := orderBy["direction"].(string); ok && dir == "ASC" {
			direction = 1
		}
	}
	sort.Slice(issues, func(i, j int) bool {
		return (issues[i].CreatedSeq-issues[j].CreatedSeq)*direction < 0
	})
	if afterSeq, ok := seqOfCursor(args["after"]); ok {
		filtered := issues[:0]
		for _, issue := range issues {
			if direction == 1 {
				if issue.CreatedSeq > afterSeq {
					filtered = append(filtered, issue)
				}
			} else {
				if issue.CreatedSeq < afterSeq {
					filtered = append(filtered, issue)
				}
			}
		}
		issues = filtered
	}
	first := int64(len(issues))
	if f, ok := toIntTrunc(args["first"]); ok {
		first = f
	}
	end := jsSliceEnd(int64(len(issues)), first)
	page := issues[:end]
	hasNextPage := int64(len(issues)) > int64(len(page))
	var endCursor any
	if len(page) > 0 {
		endCursor = cursorOf(page[len(page)-1])
	}
	nodes := make([]*gqlObject, 0, len(page))
	for _, issue := range page {
		nodes = append(nodes, gqlObj("Issue", issue))
	}
	return gqlObj("IssueConnection", issueConnectionValue{
		nodes:    nodes,
		pageInfo: gqlObj("PageInfo", pageInfoValue{hasNextPage: hasNextPage, endCursor: endCursor}),
	})
}

// --------------------------------------------------------------------- mutations

func (e *gqlExecutor) repoByGlobalID(id any) *Repo {
	parsed := parseGlobalID(id)
	if parsed == nil || parsed.typ != "Repository" {
		return nil
	}
	repoID, ok := jsParseInt(parsed.key)
	if !ok {
		return nil
	}
	for _, repo := range e.store.ListRepos() {
		if repo.ID == repoID {
			return repo
		}
	}
	return nil
}

func (e *gqlExecutor) issueByGlobalID(id any) (*Repo, *Issue) {
	parsed := parseGlobalID(id)
	if parsed == nil || parsed.typ != "Issue" {
		return nil, nil
	}
	// Issue global ids carry the issue's store id; find its repo by scanning.
	issueID, ok := jsParseInt(parsed.key)
	if !ok {
		return nil, nil
	}
	for _, repo := range e.store.ListRepos() {
		for _, issue := range repo.Issues {
			if issue.ID == issueID {
				return repo, issue
			}
		}
	}
	return nil, nil
}

func (e *gqlExecutor) pullByGlobalID(id any) (*Repo, *PullRequest) {
	parsed := parseGlobalID(id)
	if parsed == nil || parsed.typ != "PullRequest" {
		return nil, nil
	}
	pullID, ok := jsParseInt(parsed.key)
	if !ok {
		return nil, nil
	}
	for _, repo := range e.store.ListRepos() {
		for _, pull := range repo.Pulls {
			if pull.ID == pullID {
				return repo, pull
			}
		}
	}
	return nil, nil
}

func (e *gqlExecutor) mutateCreateIssue(args map[string]any) *gqlObject {
	input := asRecord(args["input"])
	repo := e.repoByGlobalID(input["repositoryId"])
	if repo == nil {
		e.addError("repository not found")
		return gqlObj("CreateIssuePayload", payloadValue{issue: nil})
	}
	issue := e.store.CreateIssue(repo, jsString(input["title"]), jsString(input["body"]), nil)
	if issue == nil {
		e.addError("could not create the issue")
		return gqlObj("CreateIssuePayload", payloadValue{issue: nil})
	}
	return gqlObj("CreateIssuePayload", payloadValue{issue: gqlObj("Issue", issue)})
}

func (e *gqlExecutor) mutateCloseIssue(args map[string]any) *gqlObject {
	input := asRecord(args["input"])
	repo, found := e.issueByGlobalID(input["issueId"])
	if found == nil {
		e.addError("issue not found")
		return gqlObj("CloseIssuePayload", payloadValue{issue: nil})
	}
	closed := "closed"
	issue := e.store.PatchIssue(repo, found.Number, &closed, nil, nil)
	if issue == nil {
		return gqlObj("CloseIssuePayload", payloadValue{issue: nil})
	}
	return gqlObj("CloseIssuePayload", payloadValue{issue: gqlObj("Issue", issue)})
}

func (e *gqlExecutor) mutateCreatePullRequest(args map[string]any) *gqlObject {
	input := asRecord(args["input"])
	repo := e.repoByGlobalID(input["repositoryId"])
	if repo == nil {
		e.addError("repository not found")
		return gqlObj("CreatePullRequestPayload", payloadValue{pullRequest: nil})
	}
	base := jsString(input["baseRefName"])
	if _, ok := input["baseRefName"].(string); !ok {
		base = repo.DefaultBranch
	}
	pull := e.store.CreatePullRequest(repo, jsString(input["title"]), jsString(input["body"]), jsString(input["headRefName"]), base)
	if pull == nil {
		e.addError("head or base branch does not exist")
		return gqlObj("CreatePullRequestPayload", payloadValue{pullRequest: nil})
	}
	return gqlObj("CreatePullRequestPayload", payloadValue{pullRequest: gqlObj("PullRequest", pullRefValue{repo: repo, pull: pull})})
}

func (e *gqlExecutor) mutateMergePullRequest(args map[string]any) *gqlObject {
	input := asRecord(args["input"])
	repo, found := e.pullByGlobalID(input["pullRequestId"])
	if found == nil {
		e.addError("pull request not found")
		return gqlObj("MergePullRequestPayload", payloadValue{pullRequest: nil})
	}
	merged := e.store.MergePull(repo, found.Number)
	if merged.Error != "" {
		e.addError("could not merge the pull request: " + merged.Error)
		return gqlObj("MergePullRequestPayload", payloadValue{pullRequest: gqlObj("PullRequest", pullRefValue{repo: repo, pull: found})})
	}
	return gqlObj("MergePullRequestPayload", payloadValue{pullRequest: gqlObj("PullRequest", pullRefValue{repo: repo, pull: found})})
}

func (e *gqlExecutor) mutateAddComment(args map[string]any) *gqlObject {
	input := asRecord(args["input"])
	repo, found := e.issueByGlobalID(input["subjectId"])
	if found == nil {
		e.addError("subject not found")
		return gqlObj("AddCommentPayload", payloadValue{commentEdge: gqlObj("CommentEdge", payloadValue{commentEdge: nil})})
	}
	comment := e.store.AddComment(repo, found.Number, jsString(input["body"]))
	if comment == nil {
		e.addError("could not add the comment")
		return gqlObj("AddCommentPayload", payloadValue{commentEdge: gqlObj("CommentEdge", payloadValue{commentEdge: nil})})
	}
	node := gqlObj("IssueComment", commentValue{id: comment.ID, body: comment.Body, repoID: repo.ID})
	return gqlObj("AddCommentPayload", payloadValue{commentEdge: gqlObj("CommentEdge", payloadValue{commentEdge: node})})
}

// --------------------------------------------------------------------------- handler

// executeOne executes one {query, variables, operationName?} envelope; always
// HTTP 200 on a parsed body.
func executeOneGQL(store *Store, envelope any) map[string]any {
	record := asRecord(envelope)
	query, ok := record["query"].(string)
	if !ok {
		return map[string]any{"data": nil, "errors": []any{map[string]any{"message": "query must be a string"}}}
	}
	variables := asRecord(record["variables"])
	tokens, err := lexGQL(query)
	if err != nil {
		return map[string]any{"data": nil, "errors": []any{map[string]any{"message": err.Error()}}}
	}
	operations, err := (&gqlParser{tokens: tokens}).parseDocument()
	if err != nil {
		return map[string]any{"data": nil, "errors": []any{map[string]any{"message": err.Error()}}}
	}
	if len(operations) == 0 {
		return map[string]any{"data": nil, "errors": []any{map[string]any{"message": "no operation in the document"}}}
	}
	operationName, _ := record["operationName"].(string)
	hasOperationName := false
	if _, ok := record["operationName"].(string); ok {
		hasOperationName = true
	}
	var operation *operationNode
	if hasOperationName {
		for _, candidate := range operations {
			if candidate.name == operationName {
				operation = candidate
				break
			}
		}
	} else {
		operation = operations[0]
	}
	if operation == nil {
		return map[string]any{"data": nil, "errors": []any{map[string]any{"message": "unknown operation " + operationName}}}
	}
	executor := &gqlExecutor{store: store}
	data := executor.execute(operation, variables)
	if len(executor.errors) > 0 {
		errs := make([]any, len(executor.errors))
		for i, e := range executor.errors {
			errs[i] = e
		}
		return map[string]any{"data": data, "errors": errs}
	}
	return map[string]any{"data": data}
}

// HandleGraphQL serves POST /api/graphql: a single envelope or a batch (a JSON
// array, answered as an array in request order). A body that is not JSON is a
// 400.
func HandleGraphQL(store *Store, body []byte) RestReply {
	parsed, err := parseJSONBody(body)
	if err != nil {
		return RestReply{Status: 400, JSON: map[string]any{"message": "Problems parsing JSON"}}
	}
	if batch, ok := parsed.([]any); ok {
		results := make([]any, len(batch))
		for i, envelope := range batch {
			results[i] = executeOneGQL(store, envelope)
		}
		return RestReply{Status: 200, JSON: results}
	}
	return RestReply{Status: 200, JSON: executeOneGQL(store, parsed)}
}
