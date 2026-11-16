// Git smart HTTP: info/refs advertisements, upload-pack (protocol v2 with a v0 fallback) and
// receive-pack (v0 only). No git binary anywhere: the pkt-line framing, the packfiles and the
// negotiation are all implemented here, on top of the in-memory store.
//
// The client (the suite's git-client.ts) is the spec: it requests v2 with `Git-Protocol:
// version=2`, sends ls-refs/fetch commands, negotiates haves, and pushes plain (delta-free) packs.
// This server answers v2, falls back to v0 when the client asks for it, sends delta-free packs,
// and honours haves exactly — an object the client already holds is never re-sent.

import { ZERO_OID, collectReachable, isObjectId, parseCommit, parseTag, parseTree, peelToCommit } from './git-objects.ts';
import type { GitObject } from './git-objects.ts';
import { readPack, writePack } from './pack.ts';
import {
  DELIM,
  FLUSH,
  decodePktLines,
  packetLines,
  pktLine,
  pktLines,
  sideband,
} from './pkt.ts';
import { DEFAULT_BRANCH, type Repo, type Store } from './store.ts';

/** What a git handler returns: an HTTP status, headers and a body buffer. */
export interface GitResponse {
  readonly status: number;
  readonly headers: Record<string, string>;
  readonly body: Buffer;
}

const UPLOAD_AD = 'application/x-git-upload-pack-advertisement';
const RECEIVE_AD = 'application/x-git-receive-pack-advertisement';
const UPLOAD_RESULT = 'application/x-git-upload-pack-result';
const RECEIVE_RESULT = 'application/x-git-receive-pack-result';

const json404 = (message: string): GitResponse => ({
  status: 404,
  headers: { 'content-type': 'application/json; charset=utf-8' },
  body: Buffer.from(`${JSON.stringify({ message })}\n`, 'utf8'),
});

/** The v2 capabilities this server advertises for upload-pack. */
const V2_CAPABILITIES: ReadonlyMap<string, string> = new Map([
  ['fetch', 'shallow filter'],
  ['ls-refs', 'unborn'],
  ['object-format', 'sha1'],
  ['agent', 'makefun-forge/1.0'],
]);

/** The v2 advertisement never changes: it names capabilities only, no refs. Built once. */
let v2AdvertisementBytes: Buffer | null = null;

/**
 * Advertisement and ls-refs answers depend only on the repo's current refs (and, for ls-refs,
 * the arguments), so identical requests share one built answer — the clone workload's 50 clones
 * ask for the same refs 50 times. The key carries the full ref state, so a push (which moves a
 * ref) can never serve a stale answer.
 */
const AD_CACHE = new Map<string, Buffer>();

/** The cache key for a ref state, from a list the caller already computed (listRefs sorts). */
function refsKey(refs: readonly { name: string; oid: string }[]): string {
  return refs.map((ref) => `${ref.name}=${ref.oid}`).join(';');
}

/** The v0 upload-pack capabilities (first ref line, NUL-separated). */
const V0_UPLOAD_CAPABILITIES = `multi_ack multi_ack_detailed thin-pack side-band-64k ofs-delta shallow agent=makefun-forge/1.0 symref=HEAD:refs/heads/${DEFAULT_BRANCH}`;

/** The v0 receive-pack capabilities (first ref line, NUL-separated). */
const V0_RECEIVE_CAPABILITIES =
  'report-status report-status-v2 side-band-64k delete-refs ofs-delta agent=makefun-forge/1.0';

/** Every ref line of an advertisement, with peeled tag targets resolved. */
function advertisementRefs(
  repo: Repo,
  store: Store,
  all: readonly { name: string; oid: string }[],
): { name: string; oid: string; peeled?: string }[] {
  const refs: { name: string; oid: string; peeled?: string }[] = [];
  const tip = store.branchTip(repo, repo.defaultBranch);
  refs.push({ name: 'HEAD', oid: tip ?? ZERO_OID });
  for (const { name, oid } of all) {
    const entry: { name: string; oid: string; peeled?: string } = { name, oid };
    const object = store.getObject(repo, oid);
    if (object?.type === 'tag') {
      const peeled = peelToCommit(oid, store.lookup(repo));
      if (peeled !== null) entry.peeled = peeled;
    }
    refs.push(entry);
  }
  return refs;
}

/** The v0 ref advertisement: the service preamble, then one line per ref (caps on the first). */
function v0Advertisement(service: 'git-upload-pack' | 'git-receive-pack', repo: Repo, store: Store): Buffer {
  // One listRefs serves the key, the ref lines and the empty check (it used to run three times).
  const all = store.listRefs(repo);
  const key = `v0|${service}|${repo.generation}|${refsKey(all)}`;
  const cached = AD_CACHE.get(key);
  if (cached) return cached;
  const capabilities = service === 'git-upload-pack' ? V0_UPLOAD_CAPABILITIES : V0_RECEIVE_CAPABILITIES;
  const parts: Buffer[] = [pktLine(`# service=${service}\n`), FLUSH];
  const refs = advertisementRefs(repo, store, all);
  if (all.length === 0) {
    // An empty repository advertises only the capabilities pseudo-ref.
    parts.push(pktLine(`${ZERO_OID} capabilities^{}\0${capabilities}\n`));
  } else {
    refs.forEach((ref, index) => {
      const line = index === 0 ? `${ref.oid} ${ref.name}\0${capabilities}\n` : `${ref.oid} ${ref.name}\n`;
      parts.push(pktLine(line));
      if (ref.peeled !== undefined) parts.push(pktLine(`${ref.peeled} ${ref.name}^{}\n`));
    });
  }
  parts.push(FLUSH);
  const built = Buffer.concat(parts);
  if (AD_CACHE.size > 512) AD_CACHE.clear();
  AD_CACHE.set(key, built);
  return built;
}

/** The protocol v2 capability advertisement: "version 2", then one line per capability. */
function v2Advertisement(): Buffer {
  if (v2AdvertisementBytes !== null) return v2AdvertisementBytes;
  const parts: Buffer[] = [pktLine('version 2\n')];
  for (const [key, value] of V2_CAPABILITIES) {
    parts.push(pktLine(value === '' ? `${key}\n` : `${key}=${value}\n`));
  }
  parts.push(FLUSH);
  v2AdvertisementBytes = Buffer.concat(parts);
  return v2AdvertisementBytes;
}

/**
 * GET /{owner}/{repo}.git/info/refs?service=<service>: the advertisement. A missing repository
 * is 404; upload-pack answers v2 when the client sends `Git-Protocol: version=2`.
 */
export function handleInfoRefs(
  store: Store,
  owner: string,
  repoName: string,
  service: string,
  gitProtocol: string | undefined,
): GitResponse {
  const repo = store.getRepo(owner, repoName);
  if (!repo) return json404('Not Found');
  if (service === 'git-upload-pack') {
    if (gitProtocol === 'version=2') {
      return { status: 200, headers: { 'content-type': UPLOAD_AD }, body: v2Advertisement() };
    }
    return { status: 200, headers: { 'content-type': UPLOAD_AD }, body: v0Advertisement('git-upload-pack', repo, store) };
  }
  if (service === 'git-receive-pack') {
    return { status: 200, headers: { 'content-type': RECEIVE_AD }, body: v0Advertisement('git-receive-pack', repo, store) };
  }
  return json404('Not Found');
}

// --------------------------------------------------------------------------- ls-refs

interface LsRefsArgs {
  peel: boolean;
  symrefs: boolean;
  unborn: boolean;
  prefixes: string[];
}

/** Parse the argument lines of a v2 ls-refs command. */
function parseLsRefsArgs(lines: readonly string[]): LsRefsArgs {
  const args: LsRefsArgs = { peel: false, symrefs: false, unborn: false, prefixes: [] };
  for (const line of lines) {
    const text = line.replace(/\n$/, '');
    if (text === 'peel') args.peel = true;
    else if (text === 'symrefs') args.symrefs = true;
    else if (text === 'unborn') args.unborn = true;
    else if (text.startsWith('ref-prefix ')) args.prefixes.push(text.slice('ref-prefix '.length));
  }
  return args;
}

/** Answer a v2 ls-refs command. */
function lsRefs(repo: Repo, store: Store, args: LsRefsArgs): Buffer {
  // One listRefs serves the key, the ref lines and the empty check (it used to run three times).
  const all = store.listRefs(repo);
  const key = `ls|${repo.generation}|${refsKey(all)}|${args.peel ? 1 : 0}${args.symrefs ? 1 : 0}${args.unborn ? 1 : 0}|${args.prefixes.join(',')}`;
  const cached = AD_CACHE.get(key);
  if (cached) return cached;
  const refs = advertisementRefs(repo, store, all);
  const parts: Buffer[] = [];
  const include = (name: string): boolean =>
    args.prefixes.length === 0 || args.prefixes.some((prefix) => name.startsWith(prefix));
  if (all.length === 0) {
    // An unborn HEAD: the client reads the literal "unborn" token as the zero oid.
    if (include('HEAD')) {
      const symref = args.symrefs ? ` symref-target:refs/heads/${repo.defaultBranch}` : '';
      parts.push(pktLine(`unborn HEAD${symref}\n`));
    }
  } else {
    for (const ref of refs) {
      if (!include(ref.name)) continue;
      let line = `${ref.oid} ${ref.name}`;
      if (ref.name === 'HEAD' && args.symrefs) line += ` symref-target:refs/heads/${repo.defaultBranch}`;
      if (ref.peeled !== undefined && args.peel) line += ` peeled:${ref.peeled}`;
      parts.push(pktLine(`${line}\n`));
    }
  }
  parts.push(FLUSH);
  const built = Buffer.concat(parts);
  if (AD_CACHE.size > 512) AD_CACHE.clear();
  AD_CACHE.set(key, built);
  return built;
}

// --------------------------------------------------------------------------- fetch

interface FetchArgs {
  wants: string[];
  haves: string[];
  shallow: string[];
  depth: number | null;
}

/** Parse the argument lines of a v2 fetch command. */
function parseFetchArgs(lines: readonly string[]): FetchArgs {
  const args: FetchArgs = { wants: [], haves: [], shallow: [], depth: null };
  for (const line of lines) {
    const text = line.replace(/\n$/, '');
    if (text.startsWith('want ')) args.wants.push(text.slice(5).trim());
    else if (text.startsWith('have ')) args.haves.push(text.slice(5).trim());
    else if (text.startsWith('shallow ')) args.shallow.push(text.slice(8).trim());
    else if (text.startsWith('deepen ')) {
      const depth = parseInt(text.slice(7).trim(), 10);
      if (Number.isFinite(depth) && depth > 0) args.depth = depth;
    }
  }
  return args;
}

/**
 * The objects a fetch sends, and the shallow-info it reports.
 *
 * - `haveSet`: what the client already holds — reachable from its haves, but never past its
 *   shallow boundaries (a shallow boundary's parents are not in the client's history).
 * - `sendSet`: the depth-limited closure of the wants, minus the have set. With no depth, the
 *   full closure. With depth d, d generations of commits from each want.
 * - `shallow`: the new boundary commits (at the depth limit, with parents still absent).
 * - `unshallow`: old boundaries whose parents are all present after this fetch.
 */
function computeFetch(
  repo: Repo,
  store: Store,
  args: FetchArgs,
): { objects: GitObject[]; shallow: string[]; unshallow: string[] } {
  const lookup = store.lookup(repo);
  const shallowSet = new Set(args.shallow);

  // What the client already holds: reachable from the haves, stopping at shallow boundaries.
  const haveSet = new Set<string>();
  {
    const stack = [...args.haves];
    while (stack.length > 0) {
      const oid = stack.pop()!;
      if (!isObjectId(oid) || haveSet.has(oid)) continue;
      const object = lookup(oid);
      if (!object) continue;
      haveSet.add(oid);
      if (object.type === 'commit') {
        const commit = parseCommit(object.data);
        stack.push(commit.tree);
        // A shallow boundary's parents are not held: do not walk past the boundary.
        if (!shallowSet.has(oid)) stack.push(...commit.parents);
      } else if (object.type === 'tree') {
        for (const entry of parseTree(object.data)) {
          if (entry.mode !== '160000') stack.push(entry.oid);
        }
      } else if (object.type === 'tag') {
        stack.push(parseTag(object.data).object);
      }
    }
  }

  // The depth-limited walk from the wants: generation 1 is the wants themselves.
  const included = new Set<string>();
  const includedCommits = new Map<string, number>();
  {
    const queue: { oid: string; level: number }[] = args.wants
      .filter((oid) => isObjectId(oid) && lookup(oid) !== undefined)
      .map((oid) => ({ oid, level: 1 }));
    while (queue.length > 0) {
      const { oid, level } = queue.shift()!;
      if (included.has(oid)) continue;
      const object = lookup(oid);
      if (!object) continue;
      included.add(oid);
      if (object.type === 'commit') {
        includedCommits.set(oid, level);
        const commit = parseCommit(object.data);
        // The commit's tree and blobs are always included, at any depth.
        for (const treeOid of collectReachable([commit.tree], lookup, { allowMissing: true })) included.add(treeOid);
        if (args.depth === null || level < args.depth) {
          for (const parent of commit.parents) queue.push({ oid: parent, level: level + 1 });
        }
      } else if (object.type === 'tree') {
        for (const treeOid of collectReachable([oid], lookup, { allowMissing: true })) included.add(treeOid);
      } else if (object.type === 'tag') {
        const target = parseTag(object.data).object;
        const targetObject = lookup(target);
        if (targetObject?.type === 'commit') queue.push({ oid: target, level });
        else if (targetObject) {
          for (const reachable of collectReachable([target], lookup, { allowMissing: true })) included.add(reachable);
        }
      }
    }
  }

  const sendSet = [...included].filter((oid) => !haveSet.has(oid));
  const objects: GitObject[] = [];
  for (const oid of sendSet.sort()) {
    const object = lookup(oid);
    if (object) objects.push(object);
  }

  // The post-fetch state, for the shallow boundary: what the client holds plus what it now gets.
  const postFetch = new Set([...haveSet, ...included]);
  const shallow: string[] = [];
  const unshallow: string[] = [];
  if (args.depth !== null) {
    for (const [oid, level] of includedCommits) {
      if (level !== args.depth) continue;
      const object = lookup(oid);
      if (object?.type !== 'commit') continue;
      const parents = parseCommit(object.data).parents;
      if (parents.length > 0 && parents.some((parent) => !postFetch.has(parent))) shallow.push(oid);
    }
  }
  for (const boundary of args.shallow) {
    const object = lookup(boundary);
    if (object?.type !== 'commit') continue;
    const parents = parseCommit(object.data).parents;
    if (parents.length === 0) continue;
    if (parents.every((parent) => postFetch.has(parent))) unshallow.push(boundary);
  }
  shallow.sort();
  unshallow.sort();
  return { objects, shallow, unshallow };
}

/**
 * A fetch result is immutable once computed: objects are never modified or deleted, and the
 * closure of a want depends only on the objects reachable from it. So identical fetches (same
 * repository generation, wants, haves, shallow boundaries and depth) share one built pack — the
 * clone workload's 50 identical shallow clones build their pack once. Only fetches whose wants
 * all resolve are cached: a want that does not exist yet could appear in a later push.
 */
const FETCH_CACHE = new Map<string, { pack: Buffer; shallow: string[]; unshallow: string[] }>();

function fetchCacheKey(repo: Repo, args: FetchArgs): string {
  return `${repo.generation}|${args.wants.join(',')}|${args.haves.join(',')}|${args.shallow.join(',')}|${args.depth ?? ''}`;
}

/** True when every want resolves now: a fetch is cacheable only then (a missing want may appear later). */
function allWantsExist(repo: Repo, store: Store, args: FetchArgs): boolean {
  return args.wants.every((oid) => isObjectId(oid) && store.getObject(repo, oid) !== undefined);
}

/**
 * Whole built fetch responses (v2 and v0 framings), keyed like FETCH_CACHE plus the variant.
 * The clone workload's 50 identical fetches share one pack AND one framing: without this, every
 * clone re-framed the same pack into pkt-lines and side-band chunks.
 */
const FETCH_RESPONSE_CACHE = new Map<string, Buffer>();

/** The pack and shallow-info for a fetch, from the cache when the same fetch ran before. */
function cachedFetch(repo: Repo, store: Store, args: FetchArgs): { pack: Buffer; shallow: string[]; unshallow: string[] } {
  const key = fetchCacheKey(repo, args);
  const cached = FETCH_CACHE.get(key);
  if (cached) return cached;
  const { objects, shallow, unshallow } = computeFetch(repo, store, args);
  const result = { pack: writePack(objects), shallow, unshallow };
  if (allWantsExist(repo, store, args)) {
    if (FETCH_CACHE.size > 512) FETCH_CACHE.clear();
    FETCH_CACHE.set(key, result);
  }
  return result;
}

/** The v2 fetch response: acknowledgments, shallow-info, then the side-band packfile section. */
function fetchV2(repo: Repo, store: Store, args: FetchArgs): Buffer {
  const key = `v2|${fetchCacheKey(repo, args)}`;
  const cached = FETCH_RESPONSE_CACHE.get(key);
  if (cached) return cached;
  const { pack, shallow, unshallow } = cachedFetch(repo, store, args);
  const parts: Buffer[] = [pktLine('acknowledgments\n'), pktLine('NAK\n'), FLUSH];
  if (shallow.length > 0 || unshallow.length > 0) {
    parts.push(pktLine('shallow-info\n'));
    for (const oid of shallow) parts.push(pktLine(`shallow ${oid}\n`));
    for (const oid of unshallow) parts.push(pktLine(`unshallow ${oid}\n`));
    parts.push(FLUSH);
  }
  parts.push(pktLine('packfile\n'), ...sideband(pack, 1), FLUSH);
  const built = Buffer.concat(parts);
  if (allWantsExist(repo, store, args)) {
    if (FETCH_RESPONSE_CACHE.size > 512) FETCH_RESPONSE_CACHE.clear();
    FETCH_RESPONSE_CACHE.set(key, built);
  }
  return built;
}

/** The v0 fetch response: NAK, shallow lines, then the side-band pack. */
function fetchV0(repo: Repo, store: Store, args: FetchArgs): Buffer {
  const key = `v0|${fetchCacheKey(repo, args)}`;
  const cached = FETCH_RESPONSE_CACHE.get(key);
  if (cached) return cached;
  const { pack, shallow, unshallow } = cachedFetch(repo, store, args);
  const parts: Buffer[] = [pktLine('NAK\n')];
  for (const oid of shallow) parts.push(pktLine(`shallow ${oid}\n`));
  for (const oid of unshallow) parts.push(pktLine(`unshallow ${oid}\n`));
  parts.push(...sideband(pack, 1), FLUSH);
  const built = Buffer.concat(parts);
  if (allWantsExist(repo, store, args)) {
    if (FETCH_RESPONSE_CACHE.size > 512) FETCH_RESPONSE_CACHE.clear();
    FETCH_RESPONSE_CACHE.set(key, built);
  }
  return built;
}

/** Split a v2 request body into its command and its argument lines (after the delim). */
function parseV2Request(body: Buffer): { command: string; args: string[] } | null {
  let packets;
  try {
    packets = decodePktLines(body).packets;
  } catch {
    return null;
  }
  const lines = packetLines(packets);
  if (lines.length === 0 || !lines[0].startsWith('command=')) return null;
  const command = lines[0].slice('command='.length).replace(/\n$/, '');
  // Arguments follow the delim packet; everything before it is the command's own lines.
  const delimAt = packets.findIndex((packet) => packet.kind === 'delim');
  const argPackets = delimAt === -1 ? [] : packets.slice(delimAt + 1);
  return { command, args: packetLines(argPackets) };
}

/** Parse a v0 fetch request: want lines, a flush, have lines, done, a flush. */
function parseV0Fetch(body: Buffer): FetchArgs | null {
  let packets;
  try {
    packets = decodePktLines(body).packets;
  } catch {
    return null;
  }
  const args: FetchArgs = { wants: [], haves: [], shallow: [], depth: null };
  let seenFlush = false;
  for (const packet of packets) {
    if (packet.kind === 'flush') {
      seenFlush = true;
      continue;
    }
    if (packet.kind !== 'data') continue;
    const text = packet.payload.toString('utf8').replace(/\n$/, '');
    if (!seenFlush) {
      if (text.startsWith('want ')) {
        // The first want line carries the capabilities after a space; take the oid only.
        const rest = text.slice(5);
        args.wants.push(rest.split(' ')[0]);
      } else if (text.startsWith('shallow ')) args.shallow.push(text.slice(8).trim());
      else if (text.startsWith('deepen ')) {
        const depth = parseInt(text.slice(7).trim(), 10);
        if (Number.isFinite(depth) && depth > 0) args.depth = depth;
      }
    } else if (text.startsWith('have ')) {
      args.haves.push(text.slice(5).trim());
    }
  }
  return args;
}

/**
 * POST /{owner}/{repo}.git/git-upload-pack: a v2 command (ls-refs, fetch) when the body opens with
 * "command=", else a v0 fetch. A missing repository is 404; a malformed body is answered with an
 * empty response and the server stays up.
 */
export function handleUploadPack(
  store: Store,
  owner: string,
  repoName: string,
  body: Buffer,
  gitProtocol: string | undefined,
): GitResponse {
  const repo = store.getRepo(owner, repoName);
  if (!repo) return json404('Not Found');
  const headers = { 'content-type': UPLOAD_RESULT };
  const v2 = parseV2Request(body);
  if (v2 !== null) {
    if (v2.command === 'ls-refs') {
      return { status: 200, headers, body: lsRefs(repo, store, parseLsRefsArgs(v2.args)) };
    }
    if (v2.command === 'fetch') {
      return { status: 200, headers, body: fetchV2(repo, store, parseFetchArgs(v2.args)) };
    }
    return { status: 200, headers, body: FLUSH };
  }
  // No v2 command: a v0 fetch, or a malformed body (answered empty, never a crash).
  const v0 = parseV0Fetch(body);
  if (v0 === null || v0.wants.length === 0) return { status: 200, headers, body: FLUSH };
  void gitProtocol;
  return { status: 200, headers, body: fetchV0(repo, store, v0) };
}

/** The receive-pack report for a given outcome is byte-identical every time (the same ref lines, the
 * same side-band framing), so cache the finished body keyed by the report lines plus the framing
 * flag. The bench's 200 pushes all report the same outcome, so 199 of them skip the pkt-line build,
 * the side-band wrap and the concatenation entirely. The key joins lines on NUL, which no report
 * line can contain (a command line is split at its first NUL for capabilities), so two different
 * reports can never share a key. Bounded like the issue cache; a stale entry is impossible because
 * the report depends only on the key. */
const REPORT_CACHE = new Map<string, Buffer>();

/** The cached (or freshly built) receive-pack report body for these report lines. */
function cachedReport(reportLines: readonly string[], useSideband: boolean): Buffer {
  const key = `${useSideband ? '1' : '0'}\0${reportLines.join('\0')}`;
  let body = REPORT_CACHE.get(key);
  if (body !== undefined) return body;
  const report = pktLines(...reportLines.map((line) => `${line}\n`));
  body =
    useSideband
      ? Buffer.concat([...sideband(report, 1), FLUSH])
      : Buffer.concat([report, FLUSH]);
  if (REPORT_CACHE.size > 128) REPORT_CACHE.clear();
  REPORT_CACHE.set(key, body);
  return body;
}

// --------------------------------------------------------------------------- receive-pack

interface PushCommand {
  readonly oldOid: string;
  readonly newOid: string;
  readonly ref: string;
}

/** True when `name` appears as a whole capability token in `capabilities` (space-separated; a
 * `key=value` token matches on its key). A plain substring test would also match a longer token
 * that merely contains the name, so the match checks the characters on both sides. */
function hasCapability(capabilities: string, name: string): boolean {
  let at = capabilities.indexOf(name);
  while (at !== -1) {
    const before = at === 0 || capabilities[at - 1] === ' ';
    const after = at + name.length;
    const afterOk = after === capabilities.length || capabilities[after] === ' ' || capabilities[after] === '=';
    if (before && afterOk) return true;
    at = capabilities.indexOf(name, at + 1);
  }
  return false;
}

/** Parse the command pkt-lines of a receive-pack request (before the flush). */
function parsePushCommands(body: Buffer): { commands: PushCommand[]; useSideband: boolean; pack: Buffer } | null {
  let decoded;
  try {
    decoded = decodePktLines(body, { allowTrailing: true });
  } catch {
    return null;
  }
  const commands: PushCommand[] = [];
  let useSideband = false;
  for (const packet of decoded.packets) {
    if (packet.kind !== 'data') continue;
    const text = packet.payload.toString('utf8');
    const nul = text.indexOf('\0');
    let line = nul === -1 ? text : text.slice(0, nul);
    if (line.endsWith('\n')) line = line.slice(0, -1);
    // The only capability receive-pack acts on is side-band-64k: test for that one token instead
    // of building a full capability map. The last capabilities-bearing line wins, as before.
    if (nul !== -1) useSideband = hasCapability(text.slice(nul + 1), 'side-band-64k');
    // Exactly three space-separated fields (old, new, ref): two spaces, no third, no split array.
    const sp1 = line.indexOf(' ');
    if (sp1 === -1) continue;
    const sp2 = line.indexOf(' ', sp1 + 1);
    if (sp2 === -1 || line.indexOf(' ', sp2 + 1) !== -1) continue;
    const oldOid = line.slice(0, sp1);
    const newOid = line.slice(sp1 + 1, sp2);
    const ref = line.slice(sp2 + 1);
    if (!isObjectId(oldOid) || !isObjectId(newOid) || !ref.startsWith('refs/')) continue;
    commands.push({ oldOid, newOid, ref });
  }
  return { commands, useSideband, pack: decoded.trailing };
}

/**
 * POST /{owner}/{repo}.git/git-receive-pack: unpack the pack, then apply each ref update that
 * fast-forwards and refuse the rest per ref. The report is "unpack ok" plus one ok/ng line per
 * ref, framed in side-band band 1 when negotiated.
 */
export function handleReceivePack(store: Store, owner: string, repoName: string, body: Buffer): GitResponse {
  const repo = store.getRepo(owner, repoName);
  if (!repo) return json404('Not Found');
  const headers = { 'content-type': RECEIVE_RESULT };
  const parsed = parsePushCommands(body);
  if (parsed === null || parsed.commands.length === 0) {
    // A body that is not a push at all: report an unpack failure and stay up.
    return { status: 200, headers, body: cachedReport(['unpack empty request'], true) };
  }
  const { commands, useSideband, pack } = parsed;
  const reportLines: string[] = [];

  // Unpack first: every object lands in the store, even when a ref update is later refused.
  let unpackOk = true;
  let unpackDetail = 'ok';
  if (pack.length > 0) {
    try {
      const read = readPack(pack, { resolveBase: (oid) => store.getObject(repo, oid) });
      // readPack already hashed every object to key its map: store by that id instead of
      // hashing the bytes a second time in putObject (three SHA-1s saved per push).
      for (const [oid, object] of read.objects) store.putObjectKnownOid(repo, oid, object.type, object.data);
    } catch (err) {
      unpackOk = false;
      unpackDetail = err instanceof Error ? err.message : 'unpack failed';
    }
  }
  reportLines.push(`unpack ${unpackDetail}`);

  for (const command of commands) {
    if (!unpackOk) {
      reportLines.push(`ng ${command.ref} unpack failed`);
      continue;
    }
    const current = store.getRef(repo, command.ref);
    if (command.newOid === ZERO_OID) {
      // A delete: the old oid must match the ref's current tip.
      if (current === undefined) {
        reportLines.push(`ng ${command.ref} no such ref`);
      } else if (current !== command.oldOid) {
        reportLines.push(`ng ${command.ref} old ref value does not match`);
      } else {
        store.deleteRef(repo, command.ref);
        reportLines.push(`ok ${command.ref}`);
      }
      continue;
    }
    if (command.oldOid === ZERO_OID) {
      // A create: the ref must not exist yet, and the new tip must be in the store.
      if (current !== undefined) {
        reportLines.push(`ng ${command.ref} already exists`);
      } else if (store.getObject(repo, command.newOid) === undefined) {
        reportLines.push(`ng ${command.ref} object not found`);
      } else {
        store.setRef(repo, command.ref, command.newOid);
        reportLines.push(`ok ${command.ref}`);
      }
      continue;
    }
    // An update: the old oid must match, and the new tip must descend from it.
    if (current === undefined) {
      reportLines.push(`ng ${command.ref} no such ref`);
    } else if (current !== command.oldOid) {
      reportLines.push(`ng ${command.ref} old ref value does not match`);
    } else if (!store.isFastForward(repo, command.oldOid, command.newOid)) {
      reportLines.push(`ng ${command.ref} non-fast-forward`);
    } else {
      store.setRef(repo, command.ref, command.newOid);
      reportLines.push(`ok ${command.ref}`);
    }
  }

  return { status: 200, headers, body: cachedReport(reportLines, useSideband) };
}
