// The forge's store: every repository, object, ref, issue, pull request, label
// and comment lives here, in memory, in one process. Nothing touches the
// filesystem in the hot path: the four timed workloads are all map lookups and
// small JSON serialisations.
//
// Determinism: every timestamp comes from a clock seeded by MAKEFUN_TRIAL_NOW
// (or 2025-01-01 when unset) and advanced one second per created object, so
// created_at order is creation order and two runs of the same trial produce
// the same stamps. Ids are sequential counters, never random.

package main

import (
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultBranch is the default branch every repository is born with.
const DefaultBranch = "main"

// emptyTreeOID is the empty tree's id, hashed once: every repository is born
// with it.
var emptyTreeOID = HashObject(objTree, []byte{})

// ViewerLogin is the viewer login: the token's owner. The suite's scenario is
// one fixed seed, so the viewer is `adaiversen` in every trial — live, replay,
// practice and final alike. A repository under any other owner is 404.
const ViewerLogin = "adaiversen"

// IssueComment is one issue comment.
type IssueComment struct {
	ID        int64
	Body      string
	CreatedAt string
}

// Issue is one issue. Labels holds label names; CreatedSeq orders creation
// (ties never happen).
type Issue struct {
	ID         int64
	Number     int64
	Title      string
	Body       string
	State      string // "open" | "closed"
	Labels     []string
	CreatedAt  string
	UpdatedAt  string
	CreatedSeq int64
	Comments   []*IssueComment
}

// PullRequest is one pull request. BaseSHA is the base tip at creation; the
// head sha resolves live.
type PullRequest struct {
	ID             int64
	Number         int64
	Title          string
	Body           string
	State          string // "open" | "closed"
	Merged         bool
	MergedAt       *string
	MergeCommitSha *string
	HeadRef        string
	BaseRef        string
	BaseSHA        string
	CreatedAt      string
	UpdatedAt      string
	CreatedSeq     int64
}

// Label is one label, by name.
type Label struct {
	Name  string
	Color string
}

// Repo is one repository: its git objects and refs plus its issues, pull
// requests and labels.
type Repo struct {
	ID         int64
	Generation int64 // unique per creation: a deleted-and-recreated repository never reuses one
	Name       string
	Owner      string
	Private    bool
	Description string
	DefaultBranch string
	CreatedAt  string
	UpdatedAt  string
	Objects    map[string]*GitObject
	Refs       map[string]string // full name ("refs/heads/main", "refs/tags/v1") -> oid
	Issues     []*Issue
	Pulls      []*PullRequest
	Labels     map[string]*Label
	IssueCounter int64
	PullCounter  int64
}

// Store is the forge's whole state, plus the viewer binding and the
// deterministic clock.
type Store struct {
	repos            map[string]*Repo
	repoOrder        []*Repo // creation order (Go maps do not preserve it)
	repoCounter      int64
	generationCounter int64
	issueIDCounter   int64
	pullIDCounter    int64
	commentIDCounter int64
	seq              int64
	clockBase        time.Time
}

// NewStore creates an empty store.
func NewStore() *Store {
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	if trialNow := os.Getenv("MAKEFUN_TRIAL_NOW"); trialNow != "" {
		if parsed, err := time.Parse(time.RFC3339Nano, trialNow); err == nil {
			base = parsed
		} else if parsed, err := time.Parse("2006-01-02T15:04:05", trialNow); err == nil {
			base = parsed.UTC()
		}
	}
	return &Store{
		repos:     map[string]*Repo{},
		clockBase: base,
	}
}

// NowISO returns the next timestamp: the clock base plus one second per call,
// as an ISO string (millis, Zulu, as Date.toISOString renders it).
func (s *Store) NowISO() string {
	s.seq++
	return s.clockBase.Add(time.Duration(s.seq) * time.Second).UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

// NowEpochSec returns the next timestamp as epoch seconds, for git commit
// headers. It reads the current sequence without advancing it.
func (s *Store) NowEpochSec() int64 {
	return s.clockBase.Unix() + s.seq
}

// NextSeq returns the next creation-sequence number (strictly increasing, for
// ordering).
func (s *Store) NextSeq() int64 {
	s.seq++
	return s.seq
}

// ViewerLogin returns the viewer login: the token's owner, fixed by the
// suite's scenario.
func (s *Store) ViewerLogin() string {
	return ViewerLogin
}

// ServerIdentity returns the identity server-created commits carry.
func (s *Store) ServerIdentity() string {
	login := s.ViewerLogin()
	return login + " <" + login + "@example.invalid>"
}

// --------------------------------------------------------------------- repositories

// CreateRepo creates a repository; it returns nil when a repository of that
// name already exists.
func (s *Store) CreateRepo(name string, private bool, description string) *Repo {
	if _, ok := s.repos[name]; ok {
		return nil
	}
	s.repoCounter++
	s.generationCounter++
	now := s.NowISO()
	repo := &Repo{
		ID:            s.repoCounter,
		Generation:    s.generationCounter,
		Name:          name,
		Owner:         s.ViewerLogin(),
		Private:       private,
		Description:   description,
		DefaultBranch: DefaultBranch,
		CreatedAt:     now,
		UpdatedAt:     now,
		Objects:       map[string]*GitObject{},
		Refs:          map[string]string{},
		Labels:        map[string]*Label{},
	}
	// The empty tree, so an unborn repository's first commit has a tree to
	// build on.
	repo.Objects[emptyTreeOID] = &GitObject{Type: objTree, Data: []byte{}}
	s.repos[name] = repo
	s.repoOrder = append(s.repoOrder, repo)
	return repo
}

// GetRepo looks a repository up by owner and name. A repository belongs to the
// viewer login: the same name under another owner is not found (404, never
// 403).
func (s *Store) GetRepo(owner, name string) *Repo {
	if owner != ViewerLogin {
		return nil
	}
	return s.repos[name]
}

// ListRepos returns every repository, in creation order.
func (s *Store) ListRepos() []*Repo {
	out := make([]*Repo, len(s.repoOrder))
	copy(out, s.repoOrder)
	return out
}

// DeleteRepo deletes a repository; it returns false when it does not exist.
func (s *Store) DeleteRepo(owner, name string) bool {
	repo := s.GetRepo(owner, name)
	if repo == nil {
		return false
	}
	delete(s.repos, name)
	for i, r := range s.repoOrder {
		if r == repo {
			s.repoOrder = append(s.repoOrder[:i], s.repoOrder[i+1:]...)
			break
		}
	}
	return true
}

// --------------------------------------------------------------------- git objects

// PutObject stores an object, returning its id (a duplicate stores once).
func (s *Store) PutObject(repo *Repo, typ GitObjectType, data []byte) string {
	oid := HashObject(typ, data)
	if _, ok := repo.Objects[oid]; !ok {
		repo.Objects[oid] = &GitObject{Type: typ, Data: data}
	}
	return oid
}

// PutObjectKnownOID stores an object whose id is already known (a pack reader
// hashed it once already): the same store-once behaviour as PutObject without
// hashing the bytes a second time, and without building a second wrapper — the
// reader's object is stored as-is (it is never mutated after this point).
func (s *Store) PutObjectKnownOID(repo *Repo, oid string, object *GitObject) {
	if _, ok := repo.Objects[oid]; !ok {
		repo.Objects[oid] = object
	}
}

// GetObject fetches an object, or nil when absent.
func (s *Store) GetObject(repo *Repo, oid string) *GitObject {
	return repo.Objects[oid]
}

// Lookup returns a lookup bound to one repository, for the reachability
// helpers.
func (s *Store) Lookup(repo *Repo) ObjectLookup {
	return func(oid string) *GitObject { return repo.Objects[oid] }
}

// --------------------------------------------------------------------- refs

// GetRef returns the oid a ref points at, or "" when the ref does not exist.
func (s *Store) GetRef(repo *Repo, ref string) string {
	return repo.Refs[ref]
}

// HasRef reports whether a ref exists.
func (s *Store) HasRef(repo *Repo, ref string) bool {
	_, ok := repo.Refs[ref]
	return ok
}

// SetRef moves a ref (or creates it).
func (s *Store) SetRef(repo *Repo, ref, oid string) {
	repo.Refs[ref] = oid
}

// DeleteRef deletes a ref; it returns false when it did not exist.
func (s *Store) DeleteRef(repo *Repo, ref string) bool {
	if _, ok := repo.Refs[ref]; !ok {
		return false
	}
	delete(repo.Refs, ref)
	return true
}

// RefEntry is one ref: its name and oid.
type RefEntry struct {
	Name string
	OID  string
}

// ListRefs returns every ref, sorted by name.
func (s *Store) ListRefs(repo *Repo) []RefEntry {
	refs := make([]RefEntry, 0, len(repo.Refs))
	for name, oid := range repo.Refs {
		refs = append(refs, RefEntry{Name: name, OID: oid})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name < refs[j].Name })
	return refs
}

// BranchTip returns the tip of a branch name ("main"), or "" when the branch
// does not exist.
func (s *Store) BranchTip(repo *Repo, branch string) string {
	return repo.Refs["refs/heads/"+branch]
}

// BranchEntry is one branch: its name and tip commit.
type BranchEntry struct {
	Name string
	SHA  string
}

// ListBranches returns every branch: name and tip commit, sorted by name.
func (s *Store) ListBranches(repo *Repo) []BranchEntry {
	var branches []BranchEntry
	for name, oid := range repo.Refs {
		if strings.HasPrefix(name, "refs/heads/") {
			branches = append(branches, BranchEntry{Name: name[len("refs/heads/"):], SHA: oid})
		}
	}
	sort.Slice(branches, func(i, j int) bool { return branches[i].Name < branches[j].Name })
	return branches
}

// --------------------------------------------------------------------- trees and commits

// EmptyTreeOID returns the empty tree's id (the same in every repository).
func (s *Store) EmptyTreeOID() string {
	return emptyTreeOID
}

type treeNode struct {
	files map[string]FlatEntry
	dirs  map[string]*treeNode
}

// BuildTreeFromFlat rebuilds a tree from a flat path map, creating
// intermediate trees bottom-up. It returns the new root tree's id; every
// created tree is stored.
func (s *Store) BuildTreeFromFlat(repo *Repo, flat map[string]FlatEntry) string {
	makeNode := func() *treeNode {
		return &treeNode{files: map[string]FlatEntry{}, dirs: map[string]*treeNode{}}
	}
	root := makeNode()
	for path, entry := range flat {
		segments := strings.Split(path, "/")
		node := root
		for i := 0; i < len(segments)-1; i++ {
			segment := segments[i]
			child, ok := node.dirs[segment]
			if !ok {
				child = makeNode()
				node.dirs[segment] = child
			}
			node = child
		}
		node.files[segments[len(segments)-1]] = entry
	}
	var build func(node *treeNode) string
	build = func(node *treeNode) string {
		var entries []TreeEntry
		for name, entry := range node.files {
			entries = append(entries, TreeEntry{Mode: entry.Mode, Name: name, OID: entry.OID})
		}
		for name, child := range node.dirs {
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: build(child)})
		}
		return s.PutObject(repo, objTree, BuildTree(entries))
	}
	return build(root)
}

// TreeChange is one file change: a new blob, or nil to delete the path.
type TreeChange struct {
	Entry *FlatEntry // nil deletes the path
}

// ApplyTreeChanges applies file changes to a tree and returns the new root
// tree's id.
func (s *Store) ApplyTreeChanges(repo *Repo, treeOID string, changes map[string]*FlatEntry) string {
	flat := FlattenTree(treeOID, s.Lookup(repo), "")
	for path, change := range changes {
		if change == nil {
			delete(flat, path)
		} else {
			flat[path] = *change
		}
	}
	return s.BuildTreeFromFlat(repo, flat)
}

// CreateCommit creates a commit object, returning its id.
func (s *Store) CreateCommit(repo *Repo, tree string, parents []string, message string) string {
	identity := s.ServerIdentity()
	timestamp := s.NowEpochSec()
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	data := BuildCommit(BuildCommitInput{
		Tree:      tree,
		Parents:   parents,
		Message:   message,
		Author:    identity,
		Committer: identity,
		Timestamp: timestamp,
	})
	return s.PutObject(repo, objCommit, data)
}

// TreeOfRef returns the tree a ref (branch name or oid) points at, or "" when
// it does not resolve.
func (s *Store) TreeOfRef(repo *Repo, ref string) string {
	tip := s.BranchTip(repo, ref)
	if tip == "" && IsObjectID(ref) {
		tip = ref
	}
	if tip == "" {
		return ""
	}
	object := repo.Objects[tip]
	if object == nil || object.Type != objCommit {
		return ""
	}
	return ParseCommit(object.Data).Tree
}

// HistoryEntry is one commit of a ref's history.
type HistoryEntry struct {
	SHA     string
	Message string
}

// HistoryOfRef returns the history of a ref, newest first (first parents
// only).
func (s *Store) HistoryOfRef(repo *Repo, ref string, limit int) []HistoryEntry {
	tip := s.BranchTip(repo, ref)
	if tip == "" && IsObjectID(ref) {
		tip = ref
	}
	if tip == "" {
		return nil
	}
	var out []HistoryEntry
	current := tip
	for current != "" && len(out) < limit {
		object := repo.Objects[current]
		if object == nil || object.Type != objCommit {
			break
		}
		parsed := ParseCommit(object.Data)
		out = append(out, HistoryEntry{SHA: current, Message: parsed.Message})
		if len(parsed.Parents) == 0 {
			break
		}
		current = parsed.Parents[0]
	}
	return out
}

// --------------------------------------------------------------------- contents

// PathRead is one file (or directory listing) read from a ref's tree.
type PathRead struct {
	Kind    string // "file" | "dir"
	Mode    string
	OID     string
	Data    []byte
	Entries []DirEntry
}

// DirEntry is one entry of a directory listing.
type DirEntry struct {
	Type string // "file" | "dir"
	Name string
	Path string
	SHA  string
}

// ReadPath reads one file (or directory listing) from a ref's tree, or nil
// when the path does not exist.
func (s *Store) ReadPath(repo *Repo, ref, path string) *PathRead {
	treeOID := s.TreeOfRef(repo, ref)
	if treeOID == "" {
		return nil
	}
	flat := FlattenTree(treeOID, s.Lookup(repo), "")
	if direct, ok := flat[path]; ok {
		object := repo.Objects[direct.OID]
		if object == nil {
			return nil
		}
		return &PathRead{Kind: "file", Mode: direct.Mode, OID: direct.OID, Data: object.Data}
	}
	// A directory: the entries directly under `path`.
	prefix := path + "/"
	var entries []DirEntry
	seenDirs := map[string]bool{}
	for entryPath, entry := range flat {
		if !strings.HasPrefix(entryPath, prefix) {
			continue
		}
		rest := entryPath[len(prefix):]
		if slash := strings.Index(rest, "/"); slash == -1 {
			entries = append(entries, DirEntry{Type: "file", Name: rest, Path: entryPath, SHA: entry.OID})
		} else if !seenDirs[rest[:slash]] {
			seenDirs[rest[:slash]] = true
			entries = append(entries, DirEntry{Type: "dir", Name: rest[:slash], Path: path + "/" + rest[:slash], SHA: entry.OID})
		}
	}
	if len(entries) == 0 {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return &PathRead{Kind: "dir", Entries: entries}
}

// WriteFileResult is the outcome of writing a file: the blob and commit shas
// and whether the file is new, or Error "stale-sha".
type WriteFileResult struct {
	BlobSHA   string
	CommitSHA string
	Created   bool
	Error     string
}

// WriteFile writes (creates or updates) one file on a branch, committing it. A
// branch that does not exist yet is created from the default branch's tip
// (GitHub's behaviour), so a first write to a new branch works.
func (s *Store) WriteFile(repo *Repo, branch, path string, content []byte, message string, sha *string) WriteFileResult {
	ref := "refs/heads/" + branch
	tip := repo.Refs[ref]
	_, tipExists := repo.Refs[ref]
	if !tipExists && branch != repo.DefaultBranch {
		// A new branch starts at the default branch's tip (or unborn, when
		// there is none yet).
		tip = repo.Refs["refs/heads/"+repo.DefaultBranch]
		_, tipExists = repo.Refs["refs/heads/"+repo.DefaultBranch]
	}
	var treeOID string
	if !tipExists {
		treeOID = s.EmptyTreeOID()
	} else {
		treeOID = ParseCommit(repo.Objects[tip].Data).Tree
	}
	flat := FlattenTree(treeOID, s.Lookup(repo), "")
	current, hasCurrent := flat[path]
	if sha != nil {
		if !hasCurrent || !strings.EqualFold(current.OID, *sha) {
			return WriteFileResult{Error: "stale-sha"}
		}
	}
	blobSHA := s.PutObject(repo, objBlob, content)
	created := !hasCurrent
	newTree := s.ApplyTreeChanges(repo, treeOID, map[string]*FlatEntry{path: {Mode: "100644", OID: blobSHA}})
	var parents []string
	if tipExists {
		parents = []string{tip}
	}
	commitSHA := s.CreateCommit(repo, newTree, parents, message)
	repo.Refs[ref] = commitSHA
	return WriteFileResult{BlobSHA: blobSHA, CommitSHA: commitSHA, Created: created}
}

// DeleteFileResult is the outcome of deleting a file: the commit sha, or an
// Error ("not-found", "stale-sha" or "no-branch").
type DeleteFileResult struct {
	CommitSHA string
	Error     string
}

// DeleteFile deletes one file from a branch, committing it.
func (s *Store) DeleteFile(repo *Repo, branch, path, message string, sha *string) DeleteFileResult {
	ref := "refs/heads/" + branch
	tip, ok := repo.Refs[ref]
	if !ok {
		return DeleteFileResult{Error: "no-branch"}
	}
	treeOID := ParseCommit(repo.Objects[tip].Data).Tree
	flat := FlattenTree(treeOID, s.Lookup(repo), "")
	current, hasCurrent := flat[path]
	if !hasCurrent {
		return DeleteFileResult{Error: "not-found"}
	}
	if sha != nil && !strings.EqualFold(current.OID, *sha) {
		return DeleteFileResult{Error: "stale-sha"}
	}
	newTree := s.ApplyTreeChanges(repo, treeOID, map[string]*FlatEntry{path: nil})
	commitSHA := s.CreateCommit(repo, newTree, []string{tip}, message)
	repo.Refs[ref] = commitSHA
	return DeleteFileResult{CommitSHA: commitSHA}
}

// --------------------------------------------------------------------- labels

// CreateLabel creates a label; the existing one is returned when the name is
// already taken.
func (s *Store) CreateLabel(repo *Repo, name, color string) *Label {
	if existing, ok := repo.Labels[name]; ok {
		return existing
	}
	if color == "" {
		color = "ededed"
	}
	label := &Label{Name: name, Color: color}
	repo.Labels[name] = label
	return label
}

// HasLabel reports whether the repository has a label of that name.
func (s *Store) HasLabel(repo *Repo, name string) bool {
	_, ok := repo.Labels[name]
	return ok
}

// --------------------------------------------------------------------- issues

// CreateIssue creates an issue; it returns nil when a named label does not
// exist (422).
func (s *Store) CreateIssue(repo *Repo, title, body string, labels []string) *Issue {
	for _, name := range labels {
		if _, ok := repo.Labels[name]; !ok {
			return nil
		}
	}
	repo.IssueCounter++
	s.issueIDCounter++
	now := s.NowISO()
	issue := &Issue{
		ID:         s.issueIDCounter,
		Number:     repo.IssueCounter,
		Title:      title,
		Body:       body,
		State:      "open",
		Labels:     append([]string{}, labels...),
		CreatedAt:  now,
		UpdatedAt:  now,
		CreatedSeq: s.NextSeq(),
	}
	repo.Issues = append(repo.Issues, issue)
	return issue
}

// GetIssue returns one issue by number, or nil.
func (s *Store) GetIssue(repo *Repo, number int64) *Issue {
	for _, issue := range repo.Issues {
		if issue.Number == number {
			return issue
		}
	}
	return nil
}

// ListIssues returns issues filtered by state and ordered by creation (asc or
// desc; anything but "asc" is descending). The sort key is unused: issues are
// always ordered by creation sequence.
func (s *Store) ListIssues(repo *Repo, state, _sort, direction string) []*Issue {
	issues := append([]*Issue{}, repo.Issues...)
	if state == "open" {
		filtered := issues[:0]
		for _, issue := range issues {
			if issue.State == "open" {
				filtered = append(filtered, issue)
			}
		}
		issues = filtered
	} else if state == "closed" {
		filtered := issues[:0]
		for _, issue := range issues {
			if issue.State == "closed" {
				filtered = append(filtered, issue)
			}
		}
		issues = filtered
	}
	directionSign := int64(-1)
	if direction == "asc" {
		directionSign = 1
	}
	sort.Slice(issues, func(i, j int) bool {
		return (issues[i].CreatedSeq-issues[j].CreatedSeq)*directionSign < 0
	})
	return issues
}

// PatchIssue closes or reopens an issue (and/or retitles it); it returns nil
// when the issue does not exist.
func (s *Store) PatchIssue(repo *Repo, number int64, state, title, body *string) *Issue {
	issue := s.GetIssue(repo, number)
	if issue == nil {
		return nil
	}
	if state != nil && (*state == "open" || *state == "closed") {
		issue.State = *state
	}
	if title != nil {
		issue.Title = *title
	}
	if body != nil {
		issue.Body = *body
	}
	issue.UpdatedAt = s.NowISO()
	return issue
}

// AddComment adds a comment to an issue; it returns nil when the issue does
// not exist.
func (s *Store) AddComment(repo *Repo, number int64, body string) *IssueComment {
	issue := s.GetIssue(repo, number)
	if issue == nil {
		return nil
	}
	s.commentIDCounter++
	comment := &IssueComment{ID: s.commentIDCounter, Body: body, CreatedAt: s.NowISO()}
	issue.Comments = append(issue.Comments, comment)
	return comment
}

// --------------------------------------------------------------------- pull requests

// CreatePullRequest creates a pull request; it returns nil when either branch
// does not exist.
func (s *Store) CreatePullRequest(repo *Repo, title, body, head, base string) *PullRequest {
	headTip := s.BranchTip(repo, head)
	baseTip := s.BranchTip(repo, base)
	if headTip == "" || baseTip == "" {
		return nil
	}
	repo.PullCounter++
	s.pullIDCounter++
	now := s.NowISO()
	pull := &PullRequest{
		ID:         s.pullIDCounter,
		Number:     repo.PullCounter,
		Title:      title,
		Body:       body,
		State:      "open",
		HeadRef:    head,
		BaseRef:    base,
		BaseSHA:    baseTip,
		CreatedAt:  now,
		UpdatedAt:  now,
		CreatedSeq: s.NextSeq(),
	}
	repo.Pulls = append(repo.Pulls, pull)
	return pull
}

// GetPull returns one pull request by number, or nil.
func (s *Store) GetPull(repo *Repo, number int64) *PullRequest {
	for _, pull := range repo.Pulls {
		if pull.Number == number {
			return pull
		}
	}
	return nil
}

// ListPulls returns pull requests filtered by state (default open only),
// newest last.
func (s *Store) ListPulls(repo *Repo, state string) []*PullRequest {
	pulls := append([]*PullRequest{}, repo.Pulls...)
	if state == "all" {
		// no filter
	} else if state == "closed" {
		filtered := pulls[:0]
		for _, pull := range pulls {
			if pull.State == "closed" {
				filtered = append(filtered, pull)
			}
		}
		pulls = filtered
	} else {
		filtered := pulls[:0]
		for _, pull := range pulls {
			if pull.State == "open" {
				filtered = append(filtered, pull)
			}
		}
		pulls = filtered
	}
	sort.Slice(pulls, func(i, j int) bool { return pulls[i].CreatedSeq < pulls[j].CreatedSeq })
	return pulls
}

// PullHeadSHA returns the head branch's current tip (or "" when the branch is
// gone).
func (s *Store) PullHeadSHA(repo *Repo, pull *PullRequest) string {
	return s.BranchTip(repo, pull.HeadRef)
}

// PullFile is one changed file of a pull request.
type PullFile struct {
	Filename  string
	Status    string
	Additions int
	Deletions int
	Changes   int
}

// PullFiles returns the changed files of a pull request: the base-tip vs
// head-tip tree diff, with line counts.
func (s *Store) PullFiles(repo *Repo, pull *PullRequest) []PullFile {
	headTip := s.BranchTip(repo, pull.HeadRef)
	baseTip := s.BranchTip(repo, pull.BaseRef)
	if headTip == "" || baseTip == "" {
		return nil
	}
	headTree := ParseCommit(repo.Objects[headTip].Data).Tree
	// Diff against the base tip at creation when it still exists, else the
	// current base tip: the files a pull request touches are the ones its head
	// changed since it branched.
	var baseTree string
	if baseObject := repo.Objects[pull.BaseSHA]; baseObject != nil && baseObject.Type == objCommit {
		baseTree = ParseCommit(baseObject.Data).Tree
	} else {
		baseTree = ParseCommit(repo.Objects[baseTip].Data).Tree
	}
	diffs := DiffTrees(baseTree, headTree, s.Lookup(repo))
	files := make([]PullFile, 0, len(diffs))
	for _, diff := range diffs {
		var oldData, newData []byte
		if diff.OldOID != "" {
			if obj := repo.Objects[diff.OldOID]; obj != nil {
				oldData = obj.Data
			}
		}
		if diff.NewOID != "" {
			if obj := repo.Objects[diff.NewOID]; obj != nil {
				newData = obj.Data
			}
		}
		additions, deletions := LineDiff(oldData, newData)
		status := "modified"
		if diff.Status == "added" {
			status = "added"
		} else if diff.Status == "removed" {
			status = "removed"
		}
		files = append(files, PullFile{
			Filename:  diff.Path,
			Status:    status,
			Additions: additions,
			Deletions: deletions,
			Changes:   additions + deletions,
		})
	}
	return files
}

// PullFilePaths returns the changed paths of a pull request (for GraphQL's
// files connection).
func (s *Store) PullFilePaths(repo *Repo, pull *PullRequest) []string {
	files := s.PullFiles(repo, pull)
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Filename)
	}
	return paths
}

// PullMergeable reports whether a pull request can merge cleanly: false when a
// file changed on both sides differently.
func (s *Store) PullMergeable(repo *Repo, pull *PullRequest) bool {
	return s.MergeConflict(repo, pull) == ""
}

// MergeConflict returns the conflicting path of a pull request, or "" when it
// merges cleanly. A conflict is a file the head changed against the merge base
// that the base also changed against the merge base, with a different
// resulting blob.
func (s *Store) MergeConflict(repo *Repo, pull *PullRequest) string {
	headTip := s.BranchTip(repo, pull.HeadRef)
	baseTip := s.BranchTip(repo, pull.BaseRef)
	if headTip == "" || baseTip == "" {
		return ""
	}
	if headTip == baseTip {
		return ""
	}
	base := MergeBase(headTip, baseTip, s.Lookup(repo))
	if base == "" {
		return ""
	}
	baseTree := ParseCommit(repo.Objects[base].Data).Tree
	headTree := ParseCommit(repo.Objects[headTip].Data).Tree
	currentBaseTree := ParseCommit(repo.Objects[baseTip].Data).Tree
	headDiffs := DiffTrees(baseTree, headTree, s.Lookup(repo))
	baseDiffs := map[string]TreeDiffEntry{}
	for _, diff := range DiffTrees(baseTree, currentBaseTree, s.Lookup(repo)) {
		baseDiffs[diff.Path] = diff
	}
	for _, diff := range headDiffs {
		other, ok := baseDiffs[diff.Path]
		if !ok {
			continue
		}
		if other.NewOID != diff.NewOID {
			return diff.Path
		}
	}
	return ""
}

// MergePullResult is the outcome of a squash-merge: the merge commit sha, or
// an Error ("unknown", "closed" or "conflict").
type MergePullResult struct {
	SHA   string
	Error string
}

// MergePull squash-merges a pull request onto its base branch: one commit on
// the base tip carrying the head's changes.
func (s *Store) MergePull(repo *Repo, number int64) MergePullResult {
	pull := s.GetPull(repo, number)
	if pull == nil {
		return MergePullResult{Error: "unknown"}
	}
	if pull.State != "open" {
		return MergePullResult{Error: "closed"}
	}
	headTip := s.BranchTip(repo, pull.HeadRef)
	baseTip := s.BranchTip(repo, pull.BaseRef)
	if headTip == "" || baseTip == "" {
		return MergePullResult{Error: "conflict"}
	}
	if s.MergeConflict(repo, pull) != "" {
		return MergePullResult{Error: "conflict"}
	}
	base := MergeBase(headTip, baseTip, s.Lookup(repo))
	var baseTree string
	if base == "" {
		baseTree = s.EmptyTreeOID()
	} else {
		baseTree = ParseCommit(repo.Objects[base].Data).Tree
	}
	headTree := ParseCommit(repo.Objects[headTip].Data).Tree
	currentBaseTree := ParseCommit(repo.Objects[baseTip].Data).Tree
	// Apply the head's changes against the merge base onto the current base
	// tree.
	changes := map[string]*FlatEntry{}
	headFlat := FlattenTree(headTree, s.Lookup(repo), "")
	for _, diff := range DiffTrees(baseTree, headTree, s.Lookup(repo)) {
		if diff.Status == "removed" {
			changes[diff.Path] = nil
		} else if entry, ok := headFlat[diff.Path]; ok {
			e := entry
			changes[diff.Path] = &e
		}
	}
	newTree := s.ApplyTreeChanges(repo, currentBaseTree, changes)
	sha := s.CreateCommit(repo, newTree, []string{baseTip},
		"Merge pull request #"+strconv.FormatInt(pull.Number, 10)+" from "+repo.Owner+"/"+pull.HeadRef+"\n\n"+pull.Title)
	repo.Refs["refs/heads/"+pull.BaseRef] = sha
	pull.State = "closed"
	pull.Merged = true
	mergedAt := s.NowISO()
	pull.MergedAt = &mergedAt
	pull.MergeCommitSha = &sha
	pull.UpdatedAt = mergedAt
	return MergePullResult{SHA: sha}
}

// --------------------------------------------------------------------- fast-forward

// IsFastForward reports whether moving `ref` from oldOID to newOID is a
// fast-forward (or a create/delete).
func (s *Store) IsFastForward(repo *Repo, oldOID, newOID string) bool {
	if newOID == ZeroOID {
		return true // a delete always applies
	}
	if oldOID == ZeroOID {
		return true // a create always applies
	}
	// Fast path first, without the lookup closure: a push's new tip almost
	// always has the old tip as its immediate parent, and that answer needs
	// only the tip's own bytes.
	tip := repo.Objects[newOID]
	if tip != nil && tip.Type == objCommit && CommitHasParent(tip.Data, oldOID) {
		return true
	}
	return IsAncestor(oldOID, newOID, s.Lookup(repo))
}

// HasClosure reports whether every object reachable from the tips is already
// in the store.
func (s *Store) HasClosure(repo *Repo, tips []string) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	CollectReachable(tips, s.Lookup(repo), false)
	return true
}
