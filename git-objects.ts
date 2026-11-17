// Git objects: hashing, parsing and building commits, trees and tags, reachability, tree diffs.
// SHA-1 only (the suite speaks object-format sha1 and nothing else). No git binary, no fs: objects
// live in memory as {type, data} and their id is the SHA-1 of "<type> <len>\0" + data.

import { createHash, hash as cryptoHash } from 'node:crypto';

/** The four git object types. */
export type GitObjectType = 'commit' | 'tree' | 'blob' | 'tag';

/** One object, as stored: its type and its raw bytes (the id is derived, never stored twice). */
export interface GitObject {
  readonly type: GitObjectType;
  readonly data: Buffer;
}

/** The all-zero object id: an unborn ref, a delete, a missing parent. */
export const ZERO_OID = '0000000000000000000000000000000000000000';

/** True when the string is a lowercase 40-hex object id (a manual scan: the regex is hot). */
export function isObjectId(value: string): boolean {
  if (value.length !== 40) return false;
  for (let i = 0; i < 40; i += 1) {
    const code = value.charCodeAt(i);
    if (!((code >= 48 && code <= 57) || (code >= 97 && code <= 102))) return false;
  }
  return true;
}

/** The SHA-1 of "<type> <len>\0" + data, as lowercase hex. */
/** The object header (`"<type> <length>\0"`) for a (type, length) pair, cached: the bench hashes the
 * same shapes over and over (a 1 KiB blob, a small tree, a small commit), so the header Buffer is
 * built once per shape instead of once per hash. The header is only ever fed to a hash update,
 * which never mutates it, so sharing one Buffer per shape is safe. Keyed by (type, length) with
 * NO string built per lookup: one small Map per type, keyed by the numeric length. Bounded like
 * the other caches (1024 shapes per type). */
const HEADER_CACHES: Record<GitObjectType, Map<number, Buffer>> = {
  commit: new Map(),
  tree: new Map(),
  blob: new Map(),
  tag: new Map(),
};

/** The cached header Buffer for one object shape. */
function headerFor(type: GitObjectType, length: number): Buffer {
  const cache = HEADER_CACHES[type];
  let header = cache.get(length);
  if (header === undefined) {
    header = Buffer.from(`${type} ${length}\0`, 'ascii');
    if (cache.size > 1024) cache.clear();
    cache.set(length, header);
  }
  return header;
}

/**
 * The largest object hashed through the one-shot path: below it, concatenating the tiny header
 * onto the data and hashing once (crypto.hash) beats a two-update Hash object — the decisive
 * ablation (v5b vs v5b-hash, 16 paired rounds x 6,050 ops) measured -3.8 us/op for the one-shot
 * form, faster in 13 of 16 rounds; above it the copy would cost more than the Hash wrapper
 * saves, so the streaming form is kept. The 'hex' encoding is passed explicitly: the
 * no-encoding default differs between Node versions (a Buffer on Node 22, a hex string on
 * Node 24).
 */
const ONE_SHOT_MAX = 8192;

export function hashObject(type: GitObjectType, data: Buffer): string {
  const header = headerFor(type, data.length);
  if (data.length <= ONE_SHOT_MAX) {
    return cryptoHash('sha1', Buffer.concat([header, data]), 'hex');
  }
  return createHash('sha1').update(header).update(data).digest('hex');
}

/** The loose-object bytes: header + content (used only to verify a hash, never written to disk). */
export function encodeLooseObject(type: GitObjectType, data: Buffer): Buffer {
  return Buffer.concat([headerFor(type, data.length), data]);
}

// --------------------------------------------------------------------------- trees

/** One entry of a tree object. */
export interface TreeEntry {
  /** "100644", "100755", "120000", "40000" (tree), "160000" (gitlink). */
  readonly mode: string;
  readonly name: string;
  readonly oid: string;
}

/** Parse a tree's raw bytes into entries, in stored order. */
export function parseTree(data: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let offset = 0;
  while (offset < data.length) {
    const space = data.indexOf(0x20, offset);
    if (space === -1) throw new Error('tree-parse: entry without a mode');
    const mode = data.subarray(offset, space).toString('ascii');
    if (!/^[0-7]+$/.test(mode)) throw new Error(`tree-parse: bad mode ${JSON.stringify(mode)}`);
    const nul = data.indexOf(0x00, space + 1);
    if (nul === -1 || nul + 21 > data.length) throw new Error('tree-parse: truncated entry');
    const name = data.subarray(space + 1, nul).toString('utf8');
    const oid = data.subarray(nul + 1, nul + 21).toString('hex');
    entries.push({ mode, name, oid });
    offset = nul + 21;
  }
  return entries;
}

/** Git's tree sort: entries ordered by name, directories compared as if the name ended in "/". */
function treeSortKey(entry: TreeEntry): string {
  return entry.mode === '40000' ? `${entry.name}/` : entry.name;
}

/** Build a tree's raw bytes from entries, sorted git's way. */
export function buildTree(entries: readonly TreeEntry[]): Buffer {
  const sorted = [...entries].sort((a, b) => Buffer.compare(Buffer.from(treeSortKey(a)), Buffer.from(treeSortKey(b))));
  const parts: Buffer[] = [];
  for (const entry of sorted) {
    if (!isObjectId(entry.oid)) throw new Error(`tree-build: bad oid ${JSON.stringify(entry.oid)}`);
    parts.push(Buffer.from(`${entry.mode} ${entry.name}\0`, 'utf8'));
    parts.push(Buffer.from(entry.oid, 'hex'));
  }
  return Buffer.concat(parts);
}

// --------------------------------------------------------------------------- commits

/** A parsed commit: its headers and its message (everything after the blank line). */
export interface ParsedCommit {
  readonly tree: string;
  readonly parents: string[];
  readonly author: string;
  readonly committer: string;
  /** Headers other than tree/parent/author/committer, kept verbatim (gpgsig, encoding, ...). */
  readonly extraHeaders: string[];
  readonly message: string;
}

/** Parse a commit's raw bytes. */
export function parseCommit(data: Buffer): ParsedCommit {
  const text = data.toString('utf8');
  const blank = text.indexOf('\n\n');
  const head = blank === -1 ? text : text.slice(0, blank);
  const message = blank === -1 ? '' : text.slice(blank + 2);
  let tree = '';
  const parents: string[] = [];
  let author = '';
  let committer = '';
  const extraHeaders: string[] = [];
  for (const line of head.split('\n')) {
    if (line.startsWith('tree ')) tree = line.slice(5).trim();
    else if (line.startsWith('parent ')) parents.push(line.slice(7).trim());
    else if (line.startsWith('author ')) author = line.slice(7);
    else if (line.startsWith('committer ')) committer = line.slice(10);
    else if (line.length > 0) extraHeaders.push(line);
  }
  if (!isObjectId(tree)) throw new Error('commit-parse: no valid tree header');
  for (const parent of parents) if (!isObjectId(parent)) throw new Error('commit-parse: bad parent oid');
  return { tree, parents, author, committer, extraHeaders, message };
}

/** Build a commit's raw bytes. */
export function buildCommit(input: {
  tree: string;
  parents?: readonly string[];
  message: string;
  author: string;
  committer?: string;
  timestamp: number;
  timezone?: string;
}): Buffer {
  const tz = input.timezone ?? '+0000';
  const committer = input.committer ?? input.author;
  const lines = [`tree ${input.tree}`];
  for (const parent of input.parents ?? []) lines.push(`parent ${parent}`);
  lines.push(`author ${input.author} ${input.timestamp} ${tz}`);
  lines.push(`committer ${committer} ${input.timestamp} ${tz}`);
  return Buffer.from(`${lines.join('\n')}\n\n${input.message}`, 'utf8');
}

// --------------------------------------------------------------------------- tags

/** A parsed annotated tag. */
export interface ParsedTag {
  readonly object: string;
  readonly type: GitObjectType;
  readonly tag: string;
  readonly tagger: string;
  readonly message: string;
}

/** Parse a tag's raw bytes. */
export function parseTag(data: Buffer): ParsedTag {
  const text = data.toString('utf8');
  const blank = text.indexOf('\n\n');
  const head = blank === -1 ? text : text.slice(0, blank);
  const message = blank === -1 ? '' : text.slice(blank + 2);
  let object = '';
  let type = '';
  let tag = '';
  let tagger = '';
  for (const line of head.split('\n')) {
    if (line.startsWith('object ')) object = line.slice(7).trim();
    else if (line.startsWith('type ')) type = line.slice(5).trim();
    else if (line.startsWith('tag ')) tag = line.slice(4).trim();
    else if (line.startsWith('tagger ')) tagger = line.slice(7);
  }
  if (!isObjectId(object)) throw new Error('tag-parse: no valid object header');
  if (type !== 'commit' && type !== 'tree' && type !== 'blob' && type !== 'tag') throw new Error(`tag-parse: bad type ${JSON.stringify(type)}`);
  return { object, type, tag, tagger, message };
}

/** Build a tag's raw bytes. */
export function buildTag(input: {
  object: string;
  type: GitObjectType;
  tag: string;
  message: string;
  tagger: string;
  timestamp: number;
  timezone?: string;
}): Buffer {
  const tz = input.timezone ?? '+0000';
  const lines = [`object ${input.object}`, `type ${input.type}`, `tag ${input.tag}`, `tagger ${input.tagger} ${input.timestamp} ${tz}`];
  return Buffer.from(`${lines.join('\n')}\n\n${input.message}`, 'utf8');
}

// --------------------------------------------------------------------------- reachability

/** A lookup over an object store: oid to object, undefined when absent. */
export type ObjectLookup = (oid: string) => GitObject | undefined;

/**
 * Every object reachable from the roots, following commits (tree + parents), trees (entries,
 * gitlinks skipped) and tags (target). Throws on a missing object unless allowMissing is set.
 */
export function collectReachable(
  roots: Iterable<string>,
  lookup: ObjectLookup,
  options: { allowMissing?: boolean } = {},
): Set<string> {
  const seen = new Set<string>();
  const stack = [...roots];
  while (stack.length > 0) {
    const oid = stack.pop()!;
    if (!isObjectId(oid) || seen.has(oid)) continue;
    const object = lookup(oid);
    if (!object) {
      if (options.allowMissing) continue;
      throw new Error(`missing-object: ${oid}`);
    }
    seen.add(oid);
    if (object.type === 'commit') {
      const commit = parseCommit(object.data);
      stack.push(commit.tree, ...commit.parents);
    } else if (object.type === 'tree') {
      for (const entry of parseTree(object.data)) {
        if (entry.mode !== '160000') stack.push(entry.oid);
      }
    } else if (object.type === 'tag') {
      stack.push(parseTag(object.data).object);
    }
  }
  return seen;
}

/** True when the byte is a lowercase-hex digit (the object-id alphabet). */
function isHexByte(byte: number): boolean {
  return (byte >= 48 && byte <= 57) || (byte >= 97 && byte <= 102);
}

/**
 * True when `oid` is one of the commit's immediate parents, read straight from its bytes: the
 * parent lines sit right after the tree line, so a byte scan finds the answer without a full
 * parse and without building any oid string. False for every other shape — a malformed commit,
 * a non-immediate ancestor, a parent list that stops before the match — and the caller falls
 * back to the full walk, so a weird-but-valid commit behaves exactly as before. The shape rules
 * mirror the old commitParentOids exactly (a "tree <40hex>\n" line, then "parent <40hex>" lines
 * of 48 bytes each, stopping at the blank line or the first non-parent line); only the
 * comparisons moved from toString+isObjectId to raw bytes.
 */
export function commitHasParent(data: Buffer, oid: string): boolean {
  if (data.length < 46) return false;
  if (data[0] !== 0x74 || data[1] !== 0x72 || data[2] !== 0x65 || data[3] !== 0x65 || data[4] !== 0x20) return false; // 'tree '
  if (data[45] !== 0x0a) return false;
  for (let i = 5; i < 45; i += 1) if (!isHexByte(data[i])) return false; // the tree oid
  let lineStart = 46;
  while (lineStart < data.length) {
    if (data[lineStart] === 0x0a) return false; // the blank line: the message follows
    if (
      lineStart + 7 > data.length ||
      data[lineStart] !== 0x70 || // 'p'
      data[lineStart + 1] !== 0x61 || // 'a'
      data[lineStart + 2] !== 0x72 || // 'r'
      data[lineStart + 3] !== 0x65 || // 'e'
      data[lineStart + 4] !== 0x6e || // 'n'
      data[lineStart + 5] !== 0x74 || // 't'
      data[lineStart + 6] !== 0x20 // ' '
    ) {
      return false; // not a parent line: the parent list is over
    }
    if (lineStart + 47 >= data.length) return false;
    let match = true;
    for (let i = 0; i < 40; i += 1) {
      const byte = data[lineStart + 7 + i];
      if (!isHexByte(byte) || byte !== oid.charCodeAt(i)) {
        match = false;
        break;
      }
    }
    if (match) return true;
    // A non-matching parent line must still be a well-formed 40-hex oid, or the old code
    // returned null (fall back): a bad byte anywhere in it means the same here.
    for (let i = 0; i < 40; i += 1) if (!isHexByte(data[lineStart + 7 + i])) return false;
    lineStart += 48; // "parent " + 40 hex + "\n"
  }
  return false;
}

/** True when `ancestor` is reachable from `tip` (or equals it) — the fast-forward test. */
export function isAncestor(ancestor: string, tip: string, lookup: ObjectLookup): boolean {
  if (ancestor === tip) return true;
  // Fast path: a push's new tip almost always has the old tip as its immediate parent. Only
  // a confirmed immediate parent answers here; every other case falls through to the walk.
  const tipObject = lookup(tip);
  if (tipObject?.type === 'commit' && commitHasParent(tipObject.data, ancestor)) return true;
  const seen = new Set<string>();
  const stack = [tip];
  while (stack.length > 0) {
    const oid = stack.pop()!;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const object = lookup(oid);
    if (!object || object.type !== 'commit') continue;
    for (const parent of parseCommit(object.data).parents) {
      if (parent === ancestor) return true;
      stack.push(parent);
    }
  }
  return false;
}

/** Peel a tag (possibly nested) to the commit it names; a commit peels to itself. */
export function peelToCommit(oid: string, lookup: ObjectLookup): string | null {
  let current = oid;
  const seen = new Set<string>();
  while (true) {
    if (seen.has(current)) return null;
    seen.add(current);
    const object = lookup(current);
    if (!object) return null;
    if (object.type === 'commit') return current;
    if (object.type === 'tag') {
      current = parseTag(object.data).object;
      continue;
    }
    return null;
  }
}

/**
 * The merge base of two commits: the first commit reachable from both, found by walking both
 * histories breadth-first from the tips (newest first). Null when the histories are disjoint.
 */
export function mergeBase(a: string, b: string, lookup: ObjectLookup): string | null {
  const fromA = new Set<string>();
  const stackA = [a];
  while (stackA.length > 0) {
    const oid = stackA.pop()!;
    if (fromA.has(oid)) continue;
    fromA.add(oid);
    const object = lookup(oid);
    if (object?.type === 'commit') stackA.push(...parseCommit(object.data).parents);
  }
  const seenB = new Set<string>();
  const stackB = [b];
  while (stackB.length > 0) {
    const oid = stackB.pop()!;
    if (seenB.has(oid)) continue;
    seenB.add(oid);
    if (fromA.has(oid)) return oid;
    const object = lookup(oid);
    if (object?.type === 'commit') stackB.push(...parseCommit(object.data).parents);
  }
  return null;
}

/** The commits of a branch's history, newest first, following first parents only. */
export function commitHistory(tip: string, lookup: ObjectLookup, limit = 1000): string[] {
  const history: string[] = [];
  let current: string | undefined = tip;
  while (current && history.length < limit) {
    const object = lookup(current);
    if (!object || object.type !== 'commit') break;
    history.push(current);
    current = parseCommit(object.data).parents[0];
  }
  return history;
}

// --------------------------------------------------------------------------- tree diffs

/** One changed path between two trees. */
export interface TreeDiffEntry {
  readonly path: string;
  /** "added" | "modified" | "removed". */
  readonly status: 'added' | 'modified' | 'removed';
  readonly oldOid: string | null;
  readonly newOid: string | null;
}

/** Flatten a tree into path → {mode, oid}, recursing into subtrees. */
export function flattenTree(treeOid: string, lookup: ObjectLookup, prefix = ''): Map<string, { mode: string; oid: string }> {
  const flat = new Map<string, { mode: string; oid: string }>();
  const walk = (oid: string, path: string): void => {
    const object = lookup(oid);
    if (!object || object.type !== 'tree') return;
    for (const entry of parseTree(object.data)) {
      const child = path === '' ? entry.name : `${path}/${entry.name}`;
      if (entry.mode === '40000') walk(entry.oid, child);
      else flat.set(child, { mode: entry.mode, oid: entry.oid });
    }
  };
  walk(treeOid, prefix);
  return flat;
}

/** The paths that differ between two trees (by oid, and by mode when the oid matches). */
export function diffTrees(oldTree: string | null, newTree: string | null, lookup: ObjectLookup): TreeDiffEntry[] {
  const oldFlat = oldTree ? flattenTree(oldTree, lookup) : new Map();
  const newFlat = newTree ? flattenTree(newTree, lookup) : new Map();
  const diffs: TreeDiffEntry[] = [];
  for (const [path, entry] of newFlat) {
    const old = oldFlat.get(path);
    if (!old) diffs.push({ path, status: 'added', oldOid: null, newOid: entry.oid });
    else if (old.oid !== entry.oid || old.mode !== entry.mode) {
      diffs.push({ path, status: 'modified', oldOid: old.oid, newOid: entry.oid });
    }
  }
  for (const [path, entry] of oldFlat) {
    if (!newFlat.has(path)) diffs.push({ path, status: 'removed', oldOid: entry.oid, newOid: null });
  }
  diffs.sort((a, b) => (a.path < b.path ? -1 : a.path > b.path ? 1 : 0));
  return diffs;
}

/** Count the lines of a blob (a trailing newline does not start a new line). */
export function countLines(data: Buffer): number {
  if (data.length === 0) return 0;
  let count = 0;
  for (let i = 0; i < data.length; i += 1) if (data[i] === 0x0a) count += 1;
  return data[data.length - 1] === 0x0a ? count : count + 1;
}

/**
 * Line additions/deletions between two blobs, by multiset difference of lines. Added files count
 * every line as an addition, removed files every line as a deletion.
 */
export function lineDiff(oldData: Buffer | null, newData: Buffer | null): { additions: number; deletions: number } {
  if (oldData === null) return { additions: newData === null ? 0 : countLines(newData), deletions: 0 };
  if (newData === null) return { additions: 0, deletions: countLines(oldData) };
  const oldLines = oldData.toString('utf8').split('\n');
  const newLines = newData.toString('utf8').split('\n');
  if (oldLines.length > 0 && oldLines[oldLines.length - 1] === '') oldLines.pop();
  if (newLines.length > 0 && newLines[newLines.length - 1] === '') newLines.pop();
  const counts = new Map<string, number>();
  for (const line of oldLines) counts.set(line, (counts.get(line) ?? 0) + 1);
  let additions = 0;
  for (const line of newLines) {
    const left = counts.get(line) ?? 0;
    if (left > 0) counts.set(line, left - 1);
    else additions += 1;
  }
  let deletions = 0;
  for (const left of counts.values()) deletions += left;
  return { additions, deletions };
}
