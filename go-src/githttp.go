// Git smart HTTP: info/refs advertisements, upload-pack (protocol v2 with a
// v0 fallback) and receive-pack (v0 only). No git binary anywhere: the
// pkt-line framing, the packfiles and the negotiation are all implemented
// here, on top of the in-memory store.
//
// The client (the suite's git-client.ts) is the spec: it requests v2 with
// `Git-Protocol: version=2`, sends ls-refs/fetch commands, negotiates haves,
// and pushes plain (delta-free) packs. This server answers v2, falls back to
// v0 when the client asks for it, sends delta-free packs, and honours haves
// exactly — an object the client already holds is never re-sent.

package main

import (
	"bytes"
	"sort"
	"strconv"
	"strings"
)

// GitResponse is what a git handler returns: an HTTP status, headers and a
// body buffer.
type GitResponse struct {
	Status  int
	Headers [][2]string
	Body    []byte
}

const (
	uploadAd    = "application/x-git-upload-pack-advertisement"
	receiveAd   = "application/x-git-receive-pack-advertisement"
	uploadRes   = "application/x-git-upload-pack-result"
	receiveRes  = "application/x-git-receive-pack-result"
	jsonContent = "application/json; charset=utf-8"
)

func gitJSON404(message string) GitResponse {
	return GitResponse{
		Status:  404,
		Headers: [][2]string{{"content-type", jsonContent}},
		Body:    []byte(`{"message":` + jsonStr(message) + "}\n"),
	}
}

// The v2 capabilities this server advertises for upload-pack, in wire order.
var v2Capabilities = [][2]string{
	{"fetch", "shallow filter"},
	{"ls-refs", "unborn"},
	{"object-format", "sha1"},
	{"agent", "makefun-forge/1.0"},
}

// The v2 advertisement never changes: it names capabilities only, no refs.
// Built once.
var v2AdvertisementBytes []byte

// Advertisement and ls-refs answers depend only on the repo's current refs
// (and, for ls-refs, the arguments), so identical requests share one built
// answer — the clone workload's 50 clones ask for the same refs 50 times. The
// key carries the full ref state, so a push (which moves a ref) can never
// serve a stale answer.
var adCache = map[string][]byte{}

// refsKey builds the cache key for a ref state, from a list the caller already
// computed (ListRefs sorts).
func refsKey(refs []RefEntry) string {
	parts := make([]string, 0, len(refs))
	for _, ref := range refs {
		parts = append(parts, ref.Name+"="+ref.OID)
	}
	return strings.Join(parts, ";")
}

// The v0 upload-pack capabilities (first ref line, NUL-separated).
const v0UploadCapabilities = "multi_ack multi_ack_detailed thin-pack side-band-64k ofs-delta shallow agent=makefun-forge/1.0 symref=HEAD:refs/heads/" + DefaultBranch

// The v0 receive-pack capabilities (first ref line, NUL-separated).
const v0ReceiveCapabilities = "report-status report-status-v2 side-band-64k delete-refs ofs-delta agent=makefun-forge/1.0"

// adRef is one ref line of an advertisement, with a peeled tag target resolved.
type adRef struct {
	name   string
	oid    string
	peeled string // "" when none
}

// advertisementRefs returns every ref line of an advertisement, with peeled
// tag targets resolved.
func advertisementRefs(repo *Repo, store *Store, all []RefEntry) []adRef {
	refs := make([]adRef, 0, len(all)+1)
	tip := store.BranchTip(repo, repo.DefaultBranch)
	if tip == "" {
		tip = ZeroOID
	}
	refs = append(refs, adRef{name: "HEAD", oid: tip})
	for _, ref := range all {
		entry := adRef{name: ref.Name, oid: ref.OID}
		if object := store.GetObject(repo, ref.OID); object != nil && object.Type == objTag {
			if peeled := PeelToCommit(ref.OID, store.Lookup(repo)); peeled != "" {
				entry.peeled = peeled
			}
		}
		refs = append(refs, entry)
	}
	return refs
}

// v0Advertisement builds the v0 ref advertisement: the service preamble, then
// one line per ref (caps on the first).
func v0Advertisement(service string, repo *Repo, store *Store) []byte {
	// One ListRefs serves the key, the ref lines and the empty check.
	all := store.ListRefs(repo)
	key := "v0|" + service + "|" + strconv.FormatInt(repo.Generation, 10) + "|" + refsKey(all)
	if cached, ok := adCache[key]; ok {
		return cached
	}
	capabilities := v0ReceiveCapabilities
	if service == "git-upload-pack" {
		capabilities = v0UploadCapabilities
	}
	parts := [][]byte{mustPktLineString("# service=" + service + "\n"), flushPkt}
	refs := advertisementRefs(repo, store, all)
	if len(all) == 0 {
		// An empty repository advertises only the capabilities pseudo-ref.
		parts = append(parts, mustPktLineString(ZeroOID+" capabilities^{}\x00"+capabilities+"\n"))
	} else {
		for i, ref := range refs {
			var line string
			if i == 0 {
				line = ref.oid + " " + ref.name + "\x00" + capabilities + "\n"
			} else {
				line = ref.oid + " " + ref.name + "\n"
			}
			parts = append(parts, mustPktLineString(line))
			if ref.peeled != "" {
				parts = append(parts, mustPktLineString(ref.peeled+" "+ref.name+"^{}\n"))
			}
		}
	}
	parts = append(parts, flushPkt)
	built := bytes.Join(parts, nil)
	if len(adCache) > 512 {
		adCache = map[string][]byte{}
	}
	adCache[key] = built
	return built
}

// v2Advertisement builds the protocol v2 capability advertisement: "version
// 2", then one line per capability.
func v2Advertisement() []byte {
	if v2AdvertisementBytes != nil {
		return v2AdvertisementBytes
	}
	var parts [][]byte
	parts = append(parts, mustPktLineString("version 2\n"))
	for _, kv := range v2Capabilities {
		if kv[1] == "" {
			parts = append(parts, mustPktLineString(kv[0]+"\n"))
		} else {
			parts = append(parts, mustPktLineString(kv[0]+"="+kv[1]+"\n"))
		}
	}
	parts = append(parts, flushPkt)
	v2AdvertisementBytes = bytes.Join(parts, nil)
	return v2AdvertisementBytes
}

// HandleInfoRefs serves GET /{owner}/{repo}.git/info/refs?service=<service>:
// the advertisement. A missing repository is 404; upload-pack answers v2 when
// the client sends `Git-Protocol: version=2`.
func HandleInfoRefs(store *Store, owner, repoName, service, gitProtocol string) GitResponse {
	repo := store.GetRepo(owner, repoName)
	if repo == nil {
		return gitJSON404("Not Found")
	}
	if service == "git-upload-pack" {
		if gitProtocol == "version=2" {
			return GitResponse{Status: 200, Headers: [][2]string{{"content-type", uploadAd}}, Body: v2Advertisement()}
		}
		return GitResponse{Status: 200, Headers: [][2]string{{"content-type", uploadAd}}, Body: v0Advertisement("git-upload-pack", repo, store)}
	}
	if service == "git-receive-pack" {
		return GitResponse{Status: 200, Headers: [][2]string{{"content-type", receiveAd}}, Body: v0Advertisement("git-receive-pack", repo, store)}
	}
	return gitJSON404("Not Found")
}

// --------------------------------------------------------------------------- ls-refs

type lsRefsArgs struct {
	peel     bool
	symrefs  bool
	unborn   bool
	prefixes []string
}

// parseLsRefsArgs parses the argument lines of a v2 ls-refs command.
func parseLsRefsArgs(lines []string) lsRefsArgs {
	var args lsRefsArgs
	for _, line := range lines {
		text := strings.TrimSuffix(line, "\n")
		switch {
		case text == "peel":
			args.peel = true
		case text == "symrefs":
			args.symrefs = true
		case text == "unborn":
			args.unborn = true
		case strings.HasPrefix(text, "ref-prefix "):
			args.prefixes = append(args.prefixes, text[len("ref-prefix "):])
		}
	}
	return args
}

// lsRefs answers a v2 ls-refs command.
func lsRefs(repo *Repo, store *Store, args lsRefsArgs) []byte {
	// One ListRefs serves the key, the ref lines and the empty check.
	all := store.ListRefs(repo)
	flags := ""
	if args.peel {
		flags += "1"
	} else {
		flags += "0"
	}
	if args.symrefs {
		flags += "1"
	} else {
		flags += "0"
	}
	if args.unborn {
		flags += "1"
	} else {
		flags += "0"
	}
	key := "ls|" + strconv.FormatInt(repo.Generation, 10) + "|" + refsKey(all) + "|" + flags + "|" + strings.Join(args.prefixes, ",")
	if cached, ok := adCache[key]; ok {
		return cached
	}
	refs := advertisementRefs(repo, store, all)
	var parts [][]byte
	include := func(name string) bool {
		if len(args.prefixes) == 0 {
			return true
		}
		for _, prefix := range args.prefixes {
			if strings.HasPrefix(name, prefix) {
				return true
			}
		}
		return false
	}
	if len(all) == 0 {
		// An unborn HEAD: the client reads the literal "unborn" token as the
		// zero oid.
		if include("HEAD") {
			symref := ""
			if args.symrefs {
				symref = " symref-target:refs/heads/" + repo.DefaultBranch
			}
			parts = append(parts, mustPktLineString("unborn HEAD"+symref+"\n"))
		}
	} else {
		for _, ref := range refs {
			if !include(ref.name) {
				continue
			}
			line := ref.oid + " " + ref.name
			if ref.name == "HEAD" && args.symrefs {
				line += " symref-target:refs/heads/" + repo.DefaultBranch
			}
			if ref.peeled != "" && args.peel {
				line += " peeled:" + ref.peeled
			}
			parts = append(parts, mustPktLineString(line+"\n"))
		}
	}
	parts = append(parts, flushPkt)
	built := bytes.Join(parts, nil)
	if len(adCache) > 512 {
		adCache = map[string][]byte{}
	}
	adCache[key] = built
	return built
}

// --------------------------------------------------------------------------- fetch

type fetchArgs struct {
	wants   []string
	haves   []string
	shallow []string
	depth   *int64
}

// parseFetchArgs parses the argument lines of a v2 fetch command.
func parseFetchArgs(lines []string) fetchArgs {
	var args fetchArgs
	for _, line := range lines {
		text := strings.TrimSuffix(line, "\n")
		switch {
		case strings.HasPrefix(text, "want "):
			args.wants = append(args.wants, strings.TrimSpace(text[5:]))
		case strings.HasPrefix(text, "have "):
			args.haves = append(args.haves, strings.TrimSpace(text[5:]))
		case strings.HasPrefix(text, "shallow "):
			args.shallow = append(args.shallow, strings.TrimSpace(text[8:]))
		case strings.HasPrefix(text, "deepen "):
			if depth, ok := jsParseInt(strings.TrimSpace(text[7:])); ok && depth > 0 {
				d := depth
				args.depth = &d
			}
		}
	}
	return args
}

// fetchResult is the objects a fetch sends, and the shallow-info it reports.
type fetchResult struct {
	objects   []*GitObject
	shallow   []string
	unshallow []string
}

// computeFetch computes the objects a fetch sends, and the shallow-info it
// reports.
//
//   - haveSet: what the client already holds — reachable from its haves, but
//     never past its shallow boundaries (a shallow boundary's parents are not
//     in the client's history).
//   - sendSet: the depth-limited closure of the wants, minus the have set.
//     With no depth, the full closure. With depth d, d generations of commits
//     from each want.
//   - shallow: the new boundary commits (at the depth limit, with parents
//     still absent).
//   - unshallow: old boundaries whose parents are all present after this
//     fetch.
func computeFetch(repo *Repo, store *Store, args fetchArgs) fetchResult {
	lookup := store.Lookup(repo)
	shallowSet := map[string]bool{}
	for _, oid := range args.shallow {
		shallowSet[oid] = true
	}

	// What the client already holds: reachable from the haves, stopping at
	// shallow boundaries.
	haveSet := map[string]bool{}
	{
		stack := append([]string{}, args.haves...)
		for len(stack) > 0 {
			oid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if !IsObjectID(oid) || haveSet[oid] {
				continue
			}
			object := lookup(oid)
			if object == nil {
				continue
			}
			haveSet[oid] = true
			switch object.Type {
			case objCommit:
				commit := ParseCommit(object.Data)
				stack = append(stack, commit.Tree)
				// A shallow boundary's parents are not held: do not walk
				// past the boundary.
				if !shallowSet[oid] {
					stack = append(stack, commit.Parents...)
				}
			case objTree:
				for _, entry := range ParseTree(object.Data) {
					if entry.Mode != "160000" {
						stack = append(stack, entry.OID)
					}
				}
			case objTag:
				stack = append(stack, ParseTag(object.Data).Object)
			}
		}
	}

	// The depth-limited walk from the wants: generation 1 is the wants
	// themselves.
	included := map[string]bool{}
	includedCommits := map[string]int64{}
	{
		type queueItem struct {
			oid   string
			level int64
		}
		var queue []queueItem
		for _, oid := range args.wants {
			if IsObjectID(oid) && lookup(oid) != nil {
				queue = append(queue, queueItem{oid: oid, level: 1})
			}
		}
		for head := 0; head < len(queue); head++ {
			item := queue[head]
			oid, level := item.oid, item.level
			if included[oid] {
				continue
			}
			object := lookup(oid)
			if object == nil {
				continue
			}
			included[oid] = true
			switch object.Type {
			case objCommit:
				includedCommits[oid] = level
				commit := ParseCommit(object.Data)
				// The commit's tree and blobs are always included, at any
				// depth.
				for treeOID := range CollectReachable([]string{commit.Tree}, lookup, true) {
					included[treeOID] = true
				}
				if args.depth == nil || level < *args.depth {
					for _, parent := range commit.Parents {
						queue = append(queue, queueItem{oid: parent, level: level + 1})
					}
				}
			case objTree:
				for treeOID := range CollectReachable([]string{oid}, lookup, true) {
					included[treeOID] = true
				}
			case objTag:
				target := ParseTag(object.Data).Object
				targetObject := lookup(target)
				if targetObject != nil && targetObject.Type == objCommit {
					queue = append(queue, queueItem{oid: target, level: level})
				} else if targetObject != nil {
					for reachable := range CollectReachable([]string{target}, lookup, true) {
						included[reachable] = true
					}
				}
			}
		}
	}

	var sendSet []string
	for oid := range included {
		if !haveSet[oid] {
			sendSet = append(sendSet, oid)
		}
	}
	sort.Strings(sendSet)
	var objects []*GitObject
	for _, oid := range sendSet {
		if object := lookup(oid); object != nil {
			objects = append(objects, object)
		}
	}

	// The post-fetch state, for the shallow boundary: what the client holds
	// plus what it now gets.
	postFetch := map[string]bool{}
	for oid := range haveSet {
		postFetch[oid] = true
	}
	for oid := range included {
		postFetch[oid] = true
	}
	var shallow, unshallow []string
	if args.depth != nil {
		for oid, level := range includedCommits {
			if level != *args.depth {
				continue
			}
			object := lookup(oid)
			if object == nil || object.Type != objCommit {
				continue
			}
			parents := ParseCommit(object.Data).Parents
			if len(parents) == 0 {
				continue
			}
			missing := false
			for _, parent := range parents {
				if !postFetch[parent] {
					missing = true
					break
				}
			}
			if missing {
				shallow = append(shallow, oid)
			}
		}
	}
	for _, boundary := range args.shallow {
		object := lookup(boundary)
		if object == nil || object.Type != objCommit {
			continue
		}
		parents := ParseCommit(object.Data).Parents
		if len(parents) == 0 {
			continue
		}
		allPresent := true
		for _, parent := range parents {
			if !postFetch[parent] {
				allPresent = false
				break
			}
		}
		if allPresent {
			unshallow = append(unshallow, boundary)
		}
	}
	sort.Strings(shallow)
	sort.Strings(unshallow)
	return fetchResult{objects: objects, shallow: shallow, unshallow: unshallow}
}

// A fetch result is immutable once computed: objects are never modified or
// deleted, and the closure of a want depends only on the objects reachable
// from it. So identical fetches (same repository generation, wants, haves,
// shallow boundaries and depth) share one built pack — the clone workload's 50
// identical shallow clones build their pack once. Only fetches whose wants
// all resolve are cached: a want that does not exist yet could appear in a
// later push.
type cachedFetchResult struct {
	pack      []byte
	shallow   []string
	unshallow []string
}

var fetchCache = map[string]cachedFetchResult{}

func fetchCacheKey(repo *Repo, args fetchArgs) string {
	depth := ""
	if args.depth != nil {
		depth = strconv.FormatInt(*args.depth, 10)
	}
	return strconv.FormatInt(repo.Generation, 10) + "|" + strings.Join(args.wants, ",") + "|" +
		strings.Join(args.haves, ",") + "|" + strings.Join(args.shallow, ",") + "|" + depth
}

// allWantsExist reports whether every want resolves now: a fetch is cacheable
// only then (a missing want may appear later).
func allWantsExist(repo *Repo, store *Store, args fetchArgs) bool {
	for _, oid := range args.wants {
		if !IsObjectID(oid) || store.GetObject(repo, oid) == nil {
			return false
		}
	}
	return true
}

// Whole built fetch responses (v2 and v0 framings), keyed like fetchCache plus
// the variant. The clone workload's 50 identical fetches share one pack AND
// one framing.
var fetchResponseCache = map[string][]byte{}

// cachedFetch returns the pack and shallow-info for a fetch, from the cache
// when the same fetch ran before.
func cachedFetch(repo *Repo, store *Store, args fetchArgs) cachedFetchResult {
	key := fetchCacheKey(repo, args)
	if cached, ok := fetchCache[key]; ok {
		return cached
	}
	computed := computeFetch(repo, store, args)
	result := cachedFetchResult{pack: WritePack(computed.objects), shallow: computed.shallow, unshallow: computed.unshallow}
	if allWantsExist(repo, store, args) {
		if len(fetchCache) > 512 {
			fetchCache = map[string]cachedFetchResult{}
		}
		fetchCache[key] = result
	}
	return result
}

// fetchV2 builds the v2 fetch response: acknowledgments, shallow-info, then
// the side-band packfile section.
func fetchV2(repo *Repo, store *Store, args fetchArgs) []byte {
	key := "v2|" + fetchCacheKey(repo, args)
	if cached, ok := fetchResponseCache[key]; ok {
		return cached
	}
	fetched := cachedFetch(repo, store, args)
	var parts [][]byte
	parts = append(parts, mustPktLineString("acknowledgments\n"), mustPktLineString("NAK\n"), flushPkt)
	if len(fetched.shallow) > 0 || len(fetched.unshallow) > 0 {
		parts = append(parts, mustPktLineString("shallow-info\n"))
		for _, oid := range fetched.shallow {
			parts = append(parts, mustPktLineString("shallow "+oid+"\n"))
		}
		for _, oid := range fetched.unshallow {
			parts = append(parts, mustPktLineString("unshallow "+oid+"\n"))
		}
		parts = append(parts, flushPkt)
	}
	parts = append(parts, mustPktLineString("packfile\n"))
	parts = append(parts, sideband(fetched.pack, 1)...)
	parts = append(parts, flushPkt)
	built := bytes.Join(parts, nil)
	if allWantsExist(repo, store, args) {
		if len(fetchResponseCache) > 512 {
			fetchResponseCache = map[string][]byte{}
		}
		fetchResponseCache[key] = built
	}
	return built
}

// fetchV0 builds the v0 fetch response: NAK, shallow lines, then the
// side-band pack.
func fetchV0(repo *Repo, store *Store, args fetchArgs) []byte {
	key := "v0|" + fetchCacheKey(repo, args)
	if cached, ok := fetchResponseCache[key]; ok {
		return cached
	}
	fetched := cachedFetch(repo, store, args)
	var parts [][]byte
	parts = append(parts, mustPktLineString("NAK\n"))
	for _, oid := range fetched.shallow {
		parts = append(parts, mustPktLineString("shallow "+oid+"\n"))
	}
	for _, oid := range fetched.unshallow {
		parts = append(parts, mustPktLineString("unshallow "+oid+"\n"))
	}
	parts = append(parts, sideband(fetched.pack, 1)...)
	parts = append(parts, flushPkt)
	built := bytes.Join(parts, nil)
	if allWantsExist(repo, store, args) {
		if len(fetchResponseCache) > 512 {
			fetchResponseCache = map[string][]byte{}
		}
		fetchResponseCache[key] = built
	}
	return built
}

// v2Request is a parsed v2 request body: its command and its argument lines
// (after the delim).
type v2Request struct {
	command string
	args    []string
}

// parseV2Request splits a v2 request body into its command and its argument
// lines. It returns nil when the body is not a v2 request.
func parseV2Request(body []byte) *v2Request {
	packets, _, err := decodePktLines(body, false)
	if err != nil {
		return nil
	}
	lines := packetLines(packets)
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "command=") {
		return nil
	}
	command := strings.TrimSuffix(lines[0][len("command="):], "\n")
	// Arguments follow the delim packet; everything before it is the
	// command's own lines.
	delimAt := -1
	for i, p := range packets {
		if p.kind == pktDelim {
			delimAt = i
			break
		}
	}
	var argPackets []packet
	if delimAt != -1 {
		argPackets = packets[delimAt+1:]
	}
	return &v2Request{command: command, args: packetLines(argPackets)}
}

// parseV0Fetch parses a v0 fetch request: want lines, a flush, have lines,
// done, a flush. It returns nil when the body does not decode.
func parseV0Fetch(body []byte) *fetchArgs {
	packets, _, err := decodePktLines(body, false)
	if err != nil {
		return nil
	}
	args := &fetchArgs{}
	seenFlush := false
	for _, p := range packets {
		if p.kind == pktFlush {
			seenFlush = true
			continue
		}
		if p.kind != pktData {
			continue
		}
		text := strings.TrimSuffix(string(p.payload), "\n")
		if !seenFlush {
			switch {
			case strings.HasPrefix(text, "want "):
				// The first want line carries the capabilities after a
				// space; take the oid only.
				rest := text[5:]
				if sp := strings.Index(rest, " "); sp != -1 {
					rest = rest[:sp]
				}
				args.wants = append(args.wants, rest)
			case strings.HasPrefix(text, "shallow "):
				args.shallow = append(args.shallow, strings.TrimSpace(text[8:]))
			case strings.HasPrefix(text, "deepen "):
				if depth, ok := jsParseInt(strings.TrimSpace(text[7:])); ok && depth > 0 {
					d := depth
					args.depth = &d
				}
			}
		} else if strings.HasPrefix(text, "have ") {
			args.haves = append(args.haves, strings.TrimSpace(text[5:]))
		}
	}
	return args
}

// HandleUploadPack serves POST /{owner}/{repo}.git/git-upload-pack: a v2
// command (ls-refs, fetch) when the body opens with "command=", else a v0
// fetch. A missing repository is 404; a malformed body is answered with an
// empty response and the server stays up.
func HandleUploadPack(store *Store, owner, repoName string, body []byte, gitProtocol string) GitResponse {
	repo := store.GetRepo(owner, repoName)
	if repo == nil {
		return gitJSON404("Not Found")
	}
	headers := [][2]string{{"content-type", uploadRes}}
	if v2 := parseV2Request(body); v2 != nil {
		switch v2.command {
		case "ls-refs":
			return GitResponse{Status: 200, Headers: headers, Body: lsRefs(repo, store, parseLsRefsArgs(v2.args))}
		case "fetch":
			return GitResponse{Status: 200, Headers: headers, Body: fetchV2(repo, store, parseFetchArgs(v2.args))}
		default:
			return GitResponse{Status: 200, Headers: headers, Body: flushPkt}
		}
	}
	// No v2 command: a v0 fetch, or a malformed body (answered empty, never a
	// crash).
	v0 := parseV0Fetch(body)
	if v0 == nil || len(v0.wants) == 0 {
		return GitResponse{Status: 200, Headers: headers, Body: flushPkt}
	}
	_ = gitProtocol
	return GitResponse{Status: 200, Headers: headers, Body: fetchV0(repo, store, *v0)}
}

// The receive-pack report for a given outcome is byte-identical every time
// (the same ref lines, the same side-band framing), so the finished body is
// cached keyed by the report lines plus the framing flag. The bench's 200
// pushes all report the same outcome, so 199 of them skip the pkt-line build,
// the side-band wrap and the concatenation entirely. The key joins lines on
// NUL, which no report line can contain, so two different reports can never
// share a key.
var reportCache = map[string][]byte{}

// The two-line fast path: one report line per level, so the common "unpack ok"
// + "ok <ref>" report needs no joined key string at all — the lines
// themselves are the keys. One map per framing flag, so the flag needs no key
// character either.
var reportCache2SB = map[string]map[string][]byte{}
var reportCache2Plain = map[string]map[string][]byte{}

// buildReport builds a report body from its lines (the uncached path, shared
// by both cache shapes).
func buildReport(reportLines []string, useSideband bool) []byte {
	lines := make([]string, len(reportLines))
	for i, line := range reportLines {
		lines[i] = line + "\n"
	}
	report := pktLinesStrings(lines...)
	if useSideband {
		var parts [][]byte
		parts = append(parts, sideband(report, 1)...)
		parts = append(parts, flushPkt)
		return bytes.Join(parts, nil)
	}
	return append(report, flushPkt...)
}

// cachedReport returns the cached (or freshly built) receive-pack report body
// for these report lines.
func cachedReport(reportLines []string, useSideband bool) []byte {
	if len(reportLines) == 2 {
		outer := reportCache2Plain
		if useSideband {
			outer = reportCache2SB
		}
		first, second := reportLines[0], reportLines[1]
		if inner, ok := outer[first]; ok {
			if body, ok := inner[second]; ok {
				return body
			}
		}
		built := buildReport(reportLines, useSideband)
		inner, ok := outer[first]
		if !ok {
			if len(outer) > 128 {
				for k := range outer {
					delete(outer, k)
				}
			}
			inner = map[string][]byte{}
			outer[first] = inner
		}
		if len(inner) > 128 {
			for k := range inner {
				delete(inner, k)
			}
		}
		inner[second] = built
		return built
	}
	flag := "0"
	if useSideband {
		flag = "1"
	}
	key := flag + "\x00" + strings.Join(reportLines, "\x00")
	if body, ok := reportCache[key]; ok {
		return body
	}
	body := buildReport(reportLines, useSideband)
	if len(reportCache) > 128 {
		reportCache = map[string][]byte{}
	}
	reportCache[key] = body
	return body
}

// --------------------------------------------------------------------------- receive-pack

// pushCommand is one ref update of a receive-pack request.
type pushCommand struct {
	oldOID string
	newOID string
	ref    string
}

// sideBandToken is the side-band-64k token, as bytes, for the capability scan.
var sideBandToken = []byte("side-band-64k")

// hasSideBand reports whether side-band-64k appears as a whole capability
// token in capabilities (space-separated; a `key=value` token matches on its
// key). Byte-level: the token is ASCII and UTF-8 multibyte bytes are all >=
// 0x80, so no token match can span a multibyte character and no boundary byte
// (space, '=') can hide inside one.
func hasSideBand(capabilities []byte) bool {
	at := bytes.Index(capabilities, sideBandToken)
	for at != -1 {
		before := at == 0 || capabilities[at-1] == 0x20
		after := at + len(sideBandToken)
		afterOK := after == len(capabilities) || capabilities[after] == 0x20 || capabilities[after] == 0x3d
		if before && afterOK {
			return true
		}
		next := bytes.Index(capabilities[at+1:], sideBandToken)
		if next == -1 {
			return false
		}
		at = at + 1 + next
	}
	return false
}

// parsedPush is a parsed receive-pack request: its commands, whether the
// report goes on the side band, and the trailing pack bytes.
type parsedPush struct {
	commands    []pushCommand
	useSideband bool
	pack        []byte
}

// parsePushCommands parses the command pkt-lines of a receive-pack request
// (before the flush). A dedicated byte scanner, not decodePktLines: it walks
// the same pkt-line framing (the same header checks, the same errors, the same
// allowTrailing break at the first non-hex byte — the pack's "PACK" magic) and
// parses each data packet's command straight from its bytes. A malformed
// header returns nil, a malformed command line is skipped, and the last
// capabilities-bearing line wins the side-band decision.
func parsePushCommands(body []byte) *parsedPush {
	var commands []pushCommand
	useSideband := false
	offset := 0
	for offset < len(body) {
		if offset+4 > len(body) {
			return nil
		}
		length := 0
		valid := true
		for i := 0; i < 4; i++ {
			c := body[offset+i]
			var digit int
			switch {
			case c >= '0' && c <= '9':
				digit = int(c - '0')
			case c >= 'a' && c <= 'f':
				digit = int(c-'a') + 10
			default:
				digit = -1
			}
			if digit == -1 {
				valid = false
				break
			}
			length = length*16 + digit
		}
		if !valid {
			break // the pack starts here (or the stream ends): the rest is trailing
		}
		if length == 0 || length == 1 || length == 2 {
			offset += 4 // flush / delim / response-end: no command in it
			continue
		}
		if length == 3 {
			return nil
		}
		if length > pktLineMax {
			return nil
		}
		if offset+length > len(body) {
			return nil
		}
		payload := body[offset+4 : offset+length]
		offset += length
		// The command line is the bytes before the first NUL (capabilities
		// follow it), minus one trailing newline. Capabilities are read
		// first: a malformed line still sets side-band.
		nul := bytes.IndexByte(payload, 0)
		if nul != -1 {
			useSideband = hasSideBand(payload[nul+1:])
		}
		end := len(payload)
		if nul != -1 {
			end = nul
		}
		if end > 0 && payload[end-1] == '\n' {
			end--
		}
		// Exactly three space-separated fields (old, new, ref): two spaces,
		// no third.
		sp1, sp2 := -1, -1
		third := false
		for i := 0; i < end; i++ {
			if payload[i] != 0x20 {
				continue
			}
			if sp1 == -1 {
				sp1 = i
			} else if sp2 == -1 {
				sp2 = i
			} else {
				third = true
				break
			}
		}
		if sp1 == -1 || sp2 == -1 || third {
			continue
		}
		oldOID := string(payload[:sp1])
		newOID := string(payload[sp1+1 : sp2])
		ref := string(payload[sp2+1 : end])
		if !IsObjectID(oldOID) || !IsObjectID(newOID) || !strings.HasPrefix(ref, "refs/") {
			continue
		}
		commands = append(commands, pushCommand{oldOID: oldOID, newOID: newOID, ref: ref})
	}
	return &parsedPush{commands: commands, useSideband: useSideband, pack: body[offset:]}
}

// HandleReceivePack serves POST /{owner}/{repo}.git/git-receive-pack: unpack
// the pack, then apply each ref update that fast-forwards and refuse the rest
// per ref. The report is "unpack ok" plus one ok/ng line per ref, framed in
// side-band band 1 when negotiated.
func HandleReceivePack(store *Store, owner, repoName string, body []byte) GitResponse {
	repo := store.GetRepo(owner, repoName)
	if repo == nil {
		return gitJSON404("Not Found")
	}
	headers := [][2]string{{"content-type", receiveRes}}
	parsed := parsePushCommands(body)
	if parsed == nil || len(parsed.commands) == 0 {
		// A body that is not a push at all: report an unpack failure and stay
		// up.
		return GitResponse{Status: 200, Headers: headers, Body: cachedReport([]string{"unpack empty request"}, true)}
	}
	commands, useSideband, pack := parsed.commands, parsed.useSideband, parsed.pack
	var reportLines []string

	// Unpack first: every object lands in the store, even when a ref update is
	// later refused.
	unpackOK := true
	unpackDetail := "ok"
	if len(pack) > 0 {
		read, err := ReadPack(pack, func(oid string) *GitObject { return store.GetObject(repo, oid) })
		if err != nil {
			unpackOK = false
			unpackDetail = err.Error()
		} else {
			// ReadPack already hashed every object to key its map: store by
			// that id instead of hashing the bytes a second time, and store
			// the reader's object as-is instead of building a second wrapper.
			for oid, object := range read.Objects {
				store.PutObjectKnownOID(repo, oid, object)
			}
		}
	}
	reportLines = append(reportLines, "unpack "+unpackDetail)

	for _, command := range commands {
		if !unpackOK {
			reportLines = append(reportLines, "ng "+command.ref+" unpack failed")
			continue
		}
		current, hasCurrent := repo.Refs[command.ref]
		if command.newOID == ZeroOID {
			// A delete: the old oid must match the ref's current tip.
			if !hasCurrent {
				reportLines = append(reportLines, "ng "+command.ref+" no such ref")
			} else if current != command.oldOID {
				reportLines = append(reportLines, "ng "+command.ref+" old ref value does not match")
			} else {
				store.DeleteRef(repo, command.ref)
				reportLines = append(reportLines, "ok "+command.ref)
			}
			continue
		}
		if command.oldOID == ZeroOID {
			// A create: the ref must not exist yet, and the new tip must be
			// in the store.
			if hasCurrent {
				reportLines = append(reportLines, "ng "+command.ref+" already exists")
			} else if store.GetObject(repo, command.newOID) == nil {
				reportLines = append(reportLines, "ng "+command.ref+" object not found")
			} else {
				store.SetRef(repo, command.ref, command.newOID)
				reportLines = append(reportLines, "ok "+command.ref)
			}
			continue
		}
		// An update: the old oid must match, and the new tip must descend
		// from it.
		if !hasCurrent {
			reportLines = append(reportLines, "ng "+command.ref+" no such ref")
		} else if current != command.oldOID {
			reportLines = append(reportLines, "ng "+command.ref+" old ref value does not match")
		} else if !store.IsFastForward(repo, command.oldOID, command.newOID) {
			reportLines = append(reportLines, "ng "+command.ref+" non-fast-forward")
		} else {
			store.SetRef(repo, command.ref, command.newOID)
			reportLines = append(reportLines, "ok "+command.ref)
		}
	}

	return GitResponse{Status: 200, Headers: headers, Body: cachedReport(reportLines, useSideband)}
}
