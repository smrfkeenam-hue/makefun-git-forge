// Git objects: hashing, parsing and building commits, trees and tags,
// reachability, tree diffs. SHA-1 only (the suite speaks object-format sha1
// and nothing else). No git binary, no fs: objects live in memory as
// {type, data} and their id is the SHA-1 of "<type> <len>\0" + data.

package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// GitObjectType is one of the four git object types.
type GitObjectType string

const (
	objCommit GitObjectType = "commit"
	objTree   GitObjectType = "tree"
	objBlob   GitObjectType = "blob"
	objTag    GitObjectType = "tag"
)

// GitObject is one object, as stored: its type and its raw bytes (the id is
// derived, never stored twice).
type GitObject struct {
	Type GitObjectType
	Data []byte
}

// ZeroOID is the all-zero object id: an unborn ref, a delete, a missing parent.
const ZeroOID = "0000000000000000000000000000000000000000"

// IsObjectID reports whether the string is a lowercase 40-hex object id.
func IsObjectID(value string) bool {
	if len(value) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := value[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// hashHeader builds the "<type> <length>\0" header for one object shape.
func hashHeader(typ GitObjectType, length int) []byte {
	var buf []byte
	buf = append(buf, []byte(typ)...)
	buf = append(buf, ' ')
	buf = strconv.AppendInt(buf, int64(length), 10)
	buf = append(buf, 0)
	return buf
}

// oneShotMax is the largest object hashed through the one-shot path: below it,
// concatenating the tiny header onto the data and hashing once beats a
// two-update Hash object; above it the copy would cost more than the Hash
// wrapper saves.
const oneShotMax = 8192

// HashObject returns the SHA-1 of "<type> <len>\0" + data, as lowercase hex.
func HashObject(typ GitObjectType, data []byte) string {
	header := hashHeader(typ, len(data))
	if len(data) <= oneShotMax {
		combined := make([]byte, 0, len(header)+len(data))
		combined = append(combined, header...)
		combined = append(combined, data...)
		sum := sha1.Sum(combined)
		return hex.EncodeToString(sum[:])
	}
	h := sha1.New()
	h.Write(header)
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// EncodeLooseObject returns the loose-object bytes: header + content (used
// only to verify a hash, never written to disk).
func EncodeLooseObject(typ GitObjectType, data []byte) []byte {
	header := hashHeader(typ, len(data))
	out := make([]byte, 0, len(header)+len(data))
	out = append(out, header...)
	out = append(out, data...)
	return out
}

// --------------------------------------------------------------------------- trees

// TreeEntry is one entry of a tree object. Mode is "100644", "100755",
// "120000", "40000" (tree) or "160000" (gitlink).
type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

// ParseTree parses a tree's raw bytes into entries, in stored order. It panics
// on malformed input, as the Node original throws (the HTTP layer turns a
// panic into a 500, exactly like the Node dispatch catch).
func ParseTree(data []byte) []TreeEntry {
	var entries []TreeEntry
	offset := 0
	for offset < len(data) {
		space := bytes.IndexByte(data[offset:], 0x20)
		if space == -1 {
			panic("tree-parse: entry without a mode")
		}
		space += offset
		mode := string(data[offset:space])
		if !isOctalString(mode) {
			panic(fmt.Sprintf("tree-parse: bad mode %q", mode))
		}
		nul := bytes.IndexByte(data[space+1:], 0x00)
		if nul == -1 || space+1+nul+21 > len(data) {
			panic("tree-parse: truncated entry")
		}
		nul += space + 1
		name := string(data[space+1 : nul])
		oid := hex.EncodeToString(data[nul+1 : nul+21])
		entries = append(entries, TreeEntry{Mode: mode, Name: name, OID: oid})
		offset = nul + 21
	}
	return entries
}

func isOctalString(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '7' {
			return false
		}
	}
	return true
}

// treeSortKey is git's tree sort key: directories compare as if the name ended
// in "/".
func treeSortKey(entry TreeEntry) string {
	if entry.Mode == "40000" {
		return entry.Name + "/"
	}
	return entry.Name
}

// BuildTree builds a tree's raw bytes from entries, sorted git's way. It panics
// on a bad oid, as the Node original throws.
func BuildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		return bytes.Compare([]byte(treeSortKey(sorted[i])), []byte(treeSortKey(sorted[j]))) < 0
	})
	var out []byte
	for _, entry := range sorted {
		if !IsObjectID(entry.OID) {
			panic(fmt.Sprintf("tree-build: bad oid %q", entry.OID))
		}
		out = append(out, []byte(entry.Mode+" "+entry.Name+"\x00")...)
		raw, err := hex.DecodeString(entry.OID)
		if err != nil {
			panic(fmt.Sprintf("tree-build: bad oid %q", entry.OID))
		}
		out = append(out, raw...)
	}
	return out
}

// --------------------------------------------------------------------------- commits

// ParsedCommit is a parsed commit: its headers and its message (everything
// after the blank line).
type ParsedCommit struct {
	Tree         string
	Parents      []string
	Author       string
	Committer    string
	ExtraHeaders []string
	Message      string
}

// ParseCommit parses a commit's raw bytes. It panics on malformed input, as
// the Node original throws.
func ParseCommit(data []byte) *ParsedCommit {
	text := toUTF8String(data)
	blank := strings.Index(text, "\n\n")
	head := text
	message := ""
	if blank != -1 {
		head = text[:blank]
		message = text[blank+2:]
	}
	parsed := &ParsedCommit{Message: message}
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			parsed.Tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			parsed.Parents = append(parsed.Parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			parsed.Author = line[7:]
		case strings.HasPrefix(line, "committer "):
			parsed.Committer = line[10:]
		case len(line) > 0:
			parsed.ExtraHeaders = append(parsed.ExtraHeaders, line)
		}
	}
	if !IsObjectID(parsed.Tree) {
		panic("commit-parse: no valid tree header")
	}
	for _, parent := range parsed.Parents {
		if !IsObjectID(parent) {
			panic("commit-parse: bad parent oid")
		}
	}
	return parsed
}

// BuildCommitInput describes a commit to build.
type BuildCommitInput struct {
	Tree      string
	Parents   []string
	Message   string
	Author    string
	Committer string // defaults to Author when empty
	Timestamp int64
	Timezone  string // defaults to "+0000"
}

// BuildCommit builds a commit's raw bytes.
func BuildCommit(input BuildCommitInput) []byte {
	tz := input.Timezone
	if tz == "" {
		tz = "+0000"
	}
	committer := input.Committer
	if committer == "" {
		committer = input.Author
	}
	var lines []string
	lines = append(lines, "tree "+input.Tree)
	for _, parent := range input.Parents {
		lines = append(lines, "parent "+parent)
	}
	lines = append(lines, fmt.Sprintf("author %s %d %s", input.Author, input.Timestamp, tz))
	lines = append(lines, fmt.Sprintf("committer %s %d %s", committer, input.Timestamp, tz))
	return []byte(strings.Join(lines, "\n") + "\n\n" + input.Message)
}

// --------------------------------------------------------------------------- tags

// ParsedTag is a parsed annotated tag.
type ParsedTag struct {
	Object  string
	Type    GitObjectType
	Tag     string
	Tagger  string
	Message string
}

// ParseTag parses a tag's raw bytes. It panics on malformed input, as the
// Node original throws.
func ParseTag(data []byte) *ParsedTag {
	text := toUTF8String(data)
	blank := strings.Index(text, "\n\n")
	head := text
	message := ""
	if blank != -1 {
		head = text[:blank]
		message = text[blank+2:]
	}
	parsed := &ParsedTag{Message: message}
	for _, line := range strings.Split(head, "\n") {
		switch {
		case strings.HasPrefix(line, "object "):
			parsed.Object = strings.TrimSpace(line[7:])
		case strings.HasPrefix(line, "type "):
			parsed.Type = GitObjectType(strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "tag "):
			parsed.Tag = strings.TrimSpace(line[4:])
		case strings.HasPrefix(line, "tagger "):
			parsed.Tagger = line[7:]
		}
	}
	if !IsObjectID(parsed.Object) {
		panic("tag-parse: no valid object header")
	}
	switch parsed.Type {
	case objCommit, objTree, objBlob, objTag:
	default:
		panic(fmt.Sprintf("tag-parse: bad type %q", string(parsed.Type)))
	}
	return parsed
}

// BuildTagInput describes a tag to build.
type BuildTagInput struct {
	Object    string
	Type      GitObjectType
	Tag       string
	Message   string
	Tagger    string
	Timestamp int64
	Timezone  string // defaults to "+0000"
}

// BuildTag builds a tag's raw bytes.
func BuildTag(input BuildTagInput) []byte {
	tz := input.Timezone
	if tz == "" {
		tz = "+0000"
	}
	lines := []string{
		"object " + input.Object,
		"type " + string(input.Type),
		"tag " + input.Tag,
		fmt.Sprintf("tagger %s %d %s", input.Tagger, input.Timestamp, tz),
	}
	return []byte(strings.Join(lines, "\n") + "\n\n" + input.Message)
}

// --------------------------------------------------------------------------- reachability

// ObjectLookup looks an object up by oid, returning nil when absent.
type ObjectLookup func(oid string) *GitObject

// CollectReachable returns every object reachable from the roots, following
// commits (tree + parents), trees (entries, gitlinks skipped) and tags
// (target). It panics on a missing object unless allowMissing is set, as the
// Node original throws (callers that treat a missing object as a plain answer
// recover the panic).
func CollectReachable(roots []string, lookup ObjectLookup, allowMissing bool) map[string]bool {
	seen := map[string]bool{}
	stack := append([]string{}, roots...)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if !IsObjectID(oid) || seen[oid] {
			continue
		}
		object := lookup(oid)
		if object == nil {
			if allowMissing {
				continue
			}
			panic(fmt.Sprintf("missing-object: %s", oid))
		}
		seen[oid] = true
		switch object.Type {
		case objCommit:
			commit := ParseCommit(object.Data)
			stack = append(stack, commit.Tree)
			stack = append(stack, commit.Parents...)
		case objTree:
			entries := ParseTree(object.Data)
			for _, entry := range entries {
				if entry.Mode != "160000" {
					stack = append(stack, entry.OID)
				}
			}
		case objTag:
			tag := ParseTag(object.Data)
			stack = append(stack, tag.Object)
		}
	}
	return seen
}

// isHexByte reports whether the byte is a lowercase-hex digit.
func isHexByte(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f')
}

// CommitHasParent reports whether oid is one of the commit's immediate
// parents, read straight from its bytes: the parent lines sit right after the
// tree line, so a byte scan finds the answer without a full parse. False for
// every other shape — a malformed commit, a non-immediate ancestor, a parent
// list that stops before the match — and the caller falls back to the full
// walk. The shape rules mirror the old commitParentOids exactly (a
// "tree <40hex>\n" line, then "parent <40hex>" lines of 48 bytes each,
// stopping at the blank line or the first non-parent line), including its
// no-\n-check quirk: a parent line's 48th byte is never verified to be "\n".
func CommitHasParent(data []byte, oid string) bool {
	if len(data) < 46 {
		return false
	}
	if data[0] != 't' || data[1] != 'r' || data[2] != 'e' || data[3] != 'e' || data[4] != ' ' {
		return false
	}
	if data[45] != '\n' {
		return false
	}
	for i := 5; i < 45; i++ {
		if !isHexByte(data[i]) {
			return false
		}
	}
	lineStart := 46
	for lineStart < len(data) {
		if data[lineStart] == '\n' {
			return false // the blank line: the message follows
		}
		if lineStart+7 > len(data) ||
			data[lineStart] != 'p' ||
			data[lineStart+1] != 'a' ||
			data[lineStart+2] != 'r' ||
			data[lineStart+3] != 'e' ||
			data[lineStart+4] != 'n' ||
			data[lineStart+5] != 't' ||
			data[lineStart+6] != ' ' {
			return false // not a parent line: the parent list is over
		}
		if lineStart+47 >= len(data) {
			return false
		}
		match := true
		for i := 0; i < 40; i++ {
			b := data[lineStart+7+i]
			if !isHexByte(b) || b != oid[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
		// A non-matching parent line must still be a well-formed 40-hex oid,
		// or the old code returned null (fall back).
		for i := 0; i < 40; i++ {
			if !isHexByte(data[lineStart+7+i]) {
				return false
			}
		}
		lineStart += 48 // "parent " + 40 hex + "\n"
	}
	return false
}

// IsAncestor reports whether ancestor is reachable from tip (or equals it) —
// the fast-forward test.
func IsAncestor(ancestor, tip string, lookup ObjectLookup) bool {
	if ancestor == tip {
		return true
	}
	// Fast path: a push's new tip almost always has the old tip as its
	// immediate parent.
	tipObject := lookup(tip)
	if tipObject != nil && tipObject.Type == objCommit && CommitHasParent(tipObject.Data, ancestor) {
		return true
	}
	seen := map[string]bool{}
	stack := []string{tip}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		object := lookup(oid)
		if object == nil || object.Type != objCommit {
			continue
		}
		commit := ParseCommit(object.Data)
		for _, parent := range commit.Parents {
			if parent == ancestor {
				return true
			}
			stack = append(stack, parent)
		}
	}
	return false
}

// PeelToCommit peels a tag (possibly nested) to the commit it names; a commit
// peels to itself. It returns "" when the oid does not resolve to a commit.
func PeelToCommit(oid string, lookup ObjectLookup) string {
	current := oid
	seen := map[string]bool{}
	for {
		if seen[current] {
			return ""
		}
		seen[current] = true
		object := lookup(current)
		if object == nil {
			return ""
		}
		if object.Type == objCommit {
			return current
		}
		if object.Type == objTag {
			tag := ParseTag(object.Data)
			current = tag.Object
			continue
		}
		return ""
	}
}

// MergeBase returns the merge base of two commits: the first commit reachable
// from both, found by walking both histories from the tips. "" when the
// histories are disjoint.
func MergeBase(a, b string, lookup ObjectLookup) string {
	fromA := map[string]bool{}
	stackA := []string{a}
	for len(stackA) > 0 {
		oid := stackA[len(stackA)-1]
		stackA = stackA[:len(stackA)-1]
		if fromA[oid] {
			continue
		}
		fromA[oid] = true
		object := lookup(oid)
		if object != nil && object.Type == objCommit {
			commit := ParseCommit(object.Data)
			stackA = append(stackA, commit.Parents...)
		}
	}
	seenB := map[string]bool{}
	stackB := []string{b}
	for len(stackB) > 0 {
		oid := stackB[len(stackB)-1]
		stackB = stackB[:len(stackB)-1]
		if seenB[oid] {
			continue
		}
		seenB[oid] = true
		if fromA[oid] {
			return oid
		}
		object := lookup(oid)
		if object != nil && object.Type == objCommit {
			commit := ParseCommit(object.Data)
			stackB = append(stackB, commit.Parents...)
		}
	}
	return ""
}

// CommitHistory returns the commits of a branch's history, newest first,
// following first parents only.
func CommitHistory(tip string, lookup ObjectLookup, limit int) []string {
	var history []string
	current := tip
	for current != "" && len(history) < limit {
		object := lookup(current)
		if object == nil || object.Type != objCommit {
			break
		}
		history = append(history, current)
		commit := ParseCommit(object.Data)
		if len(commit.Parents) == 0 {
			break
		}
		current = commit.Parents[0]
	}
	return history
}

// --------------------------------------------------------------------------- tree diffs

// TreeDiffEntry is one changed path between two trees.
type TreeDiffEntry struct {
	Path   string
	Status string // "added" | "modified" | "removed"
	OldOID string // "" when none
	NewOID string // "" when none
}

// FlatEntry is one flattened tree path: its mode and blob oid.
type FlatEntry struct {
	Mode string
	OID  string
}

// FlattenTree flattens a tree into path -> {mode, oid}, recursing into
// subtrees.
func FlattenTree(treeOID string, lookup ObjectLookup, prefix string) map[string]FlatEntry {
	flat := map[string]FlatEntry{}
	var walk func(oid, path string)
	walk = func(oid, path string) {
		object := lookup(oid)
		if object == nil || object.Type != objTree {
			return
		}
		entries := ParseTree(object.Data)
		for _, entry := range entries {
			child := entry.Name
			if path != "" {
				child = path + "/" + entry.Name
			}
			if entry.Mode == "40000" {
				walk(entry.OID, child)
			} else {
				flat[child] = FlatEntry{Mode: entry.Mode, OID: entry.OID}
			}
		}
	}
	walk(treeOID, prefix)
	return flat
}

// DiffTrees returns the paths that differ between two trees (by oid, and by
// mode when the oid matches). A "" tree is the empty tree.
func DiffTrees(oldTree, newTree string, lookup ObjectLookup) []TreeDiffEntry {
	var oldFlat, newFlat map[string]FlatEntry
	if oldTree != "" {
		oldFlat = FlattenTree(oldTree, lookup, "")
	} else {
		oldFlat = map[string]FlatEntry{}
	}
	if newTree != "" {
		newFlat = FlattenTree(newTree, lookup, "")
	} else {
		newFlat = map[string]FlatEntry{}
	}
	var diffs []TreeDiffEntry
	for path, entry := range newFlat {
		old, ok := oldFlat[path]
		if !ok {
			diffs = append(diffs, TreeDiffEntry{Path: path, Status: "added", NewOID: entry.OID})
		} else if old.OID != entry.OID || old.Mode != entry.Mode {
			diffs = append(diffs, TreeDiffEntry{Path: path, Status: "modified", OldOID: old.OID, NewOID: entry.OID})
		}
	}
	for path, entry := range oldFlat {
		if _, ok := newFlat[path]; !ok {
			diffs = append(diffs, TreeDiffEntry{Path: path, Status: "removed", OldOID: entry.OID})
		}
	}
	sort.Slice(diffs, func(i, j int) bool { return diffs[i].Path < diffs[j].Path })
	return diffs
}

// CountLines counts the lines of a blob (a trailing newline does not start a
// new line).
func CountLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	count := 0
	for _, b := range data {
		if b == '\n' {
			count++
		}
	}
	if data[len(data)-1] == '\n' {
		return count
	}
	return count + 1
}

// LineDiff returns line additions/deletions between two blobs, by multiset
// difference of lines. Added files count every line as an addition, removed
// files every line as a deletion. A nil blob is an absent file.
func LineDiff(oldData, newData []byte) (additions, deletions int) {
	if oldData == nil {
		if newData == nil {
			return 0, 0
		}
		return CountLines(newData), 0
	}
	if newData == nil {
		return 0, CountLines(oldData)
	}
	oldLines := strings.Split(string(oldData), "\n")
	newLines := strings.Split(string(newData), "\n")
	if len(oldLines) > 0 && oldLines[len(oldLines)-1] == "" {
		oldLines = oldLines[:len(oldLines)-1]
	}
	if len(newLines) > 0 && newLines[len(newLines)-1] == "" {
		newLines = newLines[:len(newLines)-1]
	}
	counts := map[string]int{}
	for _, line := range oldLines {
		counts[line]++
	}
	for _, line := range newLines {
		if counts[line] > 0 {
			counts[line]--
		} else {
			additions++
		}
	}
	for _, left := range counts {
		deletions += left
	}
	return additions, deletions
}

// toUTF8String converts bytes to a string the way Node's Buffer.toString('utf8')
// would: invalid UTF-8 sequences become U+FFFD. Go's string([]byte) keeps
// invalid bytes as-is, so this normalises them.
func toUTF8String(data []byte) string {
	if utf8.Valid(data) {
		return string(data)
	}
	return strings.ToValidUTF8(string(data), "�")
}
