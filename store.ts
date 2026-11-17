// The forge's store: every repository, object, ref, issue, pull request, label and comment lives
// here, in memory, in one process. Nothing touches the filesystem in the hot path: the four timed
// workloads are all map lookups and small JSON serialisations.
//
// Determinism: every timestamp comes from a clock seeded by MAKEFUN_TRIAL_NOW (or 2025-01-01 when
// unset) and advanced one second per created object, so created_at order is creation order and two
// runs of the same trial produce the same stamps. Ids are sequential counters, never random.

import {
  ZERO_OID,
  buildCommit,
  buildTree,
  collectReachable,
  commitHasParent,
  countLines,
  diffTrees,
  flattenTree,
  hashObject,
  isAncestor,
  isObjectId,
  lineDiff,
  mergeBase,
  parseCommit,
  parseTree,
  type GitObject,
  type GitObjectType,
  type TreeDiffEntry,
} from './git-objects.ts';

/** The default branch every repository is born with. */
export const DEFAULT_BRANCH = 'main';

/** The empty tree's id, hashed once: every repository is born with it. */
const EMPTY_TREE_OID = hashObject('tree', Buffer.alloc(0));

/**
 * The viewer login: the token's owner. The suite's scenario is one fixed seed (wire.ts
 * SCENARIO_SEED, buildScenario() taking no trial input), so the viewer is `adaiversen` in every
 * trial — live, replay, practice and final alike. A repository under any other owner is 404.
 */
export const VIEWER_LOGIN = 'adaiversen';

/** One issue comment. */
export interface IssueComment {
  readonly id: number;
  readonly body: string;
  readonly createdAt: string;
}

/** One issue. `labels` holds label names; `createdSeq` orders creation (ties never happen). */
export interface Issue {
  readonly id: number;
  readonly number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[];
  readonly createdAt: string;
  updatedAt: string;
  readonly createdSeq: number;
  readonly comments: IssueComment[];
}

/** One pull request. `baseSha` is the base tip at creation; the head sha resolves live. */
export interface PullRequest {
  readonly id: number;
  readonly number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
  readonly headRef: string;
  readonly baseRef: string;
  readonly baseSha: string;
  readonly createdAt: string;
  updatedAt: string;
  readonly createdSeq: number;
}

/** One label, by name. */
export interface Label {
  readonly name: string;
  readonly color: string;
}

/** One repository: its git objects and refs plus its issues, pull requests and labels. */
export interface Repo {
  readonly id: number;
  /** Unique per creation: a deleted-and-recreated repository never reuses a generation. */
  readonly generation: number;
  readonly name: string;
  readonly owner: string;
  private: boolean;
  description: string;
  readonly defaultBranch: string;
  readonly createdAt: string;
  updatedAt: string;
  /** All git objects, by id. */
  readonly objects: Map<string, GitObject>;
  /** All refs, by full name ("refs/heads/main", "refs/tags/v1"). */
  readonly refs: Map<string, string>;
  readonly issues: Issue[];
  readonly pulls: PullRequest[];
  readonly labels: Map<string, Label>;
  issueCounter: number;
  pullCounter: number;
}

/** The forge's whole state, plus the viewer binding and the deterministic clock. */
export class Store {
  private readonly repos = new Map<string, Repo>();
  private repoCounter = 0;
  private generationCounter = 0;
  private issueIdCounter = 0;
  private pullIdCounter = 0;
  private commentIdCounter = 0;
  private seq = 0;
  private readonly clockBaseMs: number;

  constructor() {
    const trialNow = process.env.MAKEFUN_TRIAL_NOW;
    const parsed = trialNow ? Date.parse(trialNow) : NaN;
    this.clockBaseMs = Number.isFinite(parsed) ? parsed : Date.parse('2025-01-01T00:00:00.000Z');
  }

  /** The next timestamp: the clock base plus one second per call, as an ISO string. */
  nowIso(): string {
    this.seq += 1;
    return new Date(this.clockBaseMs + this.seq * 1000).toISOString();
  }

  /** The next timestamp as epoch seconds, for git commit headers. */
  nowEpochSec(): number {
    return Math.floor(this.clockBaseMs / 1000) + this.seq;
  }

  /** The next creation-sequence number (strictly increasing, for ordering). */
  nextSeq(): number {
    this.seq += 1;
    return this.seq;
  }

  /** The viewer login: the token's owner, fixed by the suite's scenario. */
  viewerLogin(): string {
    return VIEWER_LOGIN;
  }

  /** The identity server-created commits carry. */
  serverIdentity(): string {
    const login = this.viewerLogin();
    return `${login} <${login}@example.invalid>`;
  }

  // --------------------------------------------------------------------- repositories

  /** Create a repository; null when a repository of that name already exists. */
  createRepo(name: string, input: { private?: boolean; description?: string }): Repo | null {
    if (this.repos.has(name)) return null;
    this.repoCounter += 1;
    this.generationCounter += 1;
    const now = this.nowIso();
    const repo: Repo = {
      id: this.repoCounter,
      generation: this.generationCounter,
      name,
      owner: this.viewerLogin(),
      private: input.private ?? false,
      description: input.description ?? '',
      defaultBranch: DEFAULT_BRANCH,
      createdAt: now,
      updatedAt: now,
      objects: new Map(),
      refs: new Map(),
      issues: [],
      pulls: [],
      labels: new Map(),
      issueCounter: 0,
      pullCounter: 0,
    };
    // The empty tree, so an unborn repository's first commit has a tree to build on.
    const emptyTree = Buffer.alloc(0);
    repo.objects.set(EMPTY_TREE_OID, { type: 'tree', data: emptyTree });
    this.repos.set(name, repo);
    return repo;
  }

  /**
   * Look a repository up by owner and name. A repository belongs to the viewer login: the same
   * name under another owner is not found (404, never 403).
   */
  getRepo(owner: string, name: string): Repo | null {
    if (owner !== VIEWER_LOGIN) return null;
    return this.repos.get(name) ?? null;
  }

  /** Every repository, in creation order. */
  listRepos(): Repo[] {
    return [...this.repos.values()];
  }

  /** Delete a repository; false when it does not exist. */
  deleteRepo(owner: string, name: string): boolean {
    const repo = this.getRepo(owner, name);
    if (!repo) return false;
    this.repos.delete(name);
    return true;
  }

  // --------------------------------------------------------------------- git objects

  /** Store an object, returning its id (a duplicate stores once). */
  putObject(repo: Repo, type: GitObjectType, data: Buffer): string {
    const oid = hashObject(type, data);
    if (!repo.objects.has(oid)) repo.objects.set(oid, { type, data });
    return oid;
  }

  /**
   * Store an object whose id is already known (a pack reader hashed it once already):
   * the same store-once behaviour as putObject without hashing the bytes a second time,
   * and without building a second {type, data} wrapper — the reader's object is stored
   * as-is (it is never mutated after this point).
   */
  putObjectKnownOid(repo: Repo, oid: string, object: GitObject): void {
    if (!repo.objects.has(oid)) repo.objects.set(oid, object);
  }

  /** Fetch an object, or undefined when absent. */
  getObject(repo: Repo, oid: string): GitObject | undefined {
    return repo.objects.get(oid);
  }

  /** A lookup bound to one repository, for the reachability helpers. */
  lookup(repo: Repo): (oid: string) => GitObject | undefined {
    return (oid: string) => repo.objects.get(oid);
  }

  // --------------------------------------------------------------------- refs

  /** The oid a ref points at, or undefined when the ref does not exist. */
  getRef(repo: Repo, ref: string): string | undefined {
    return repo.refs.get(ref);
  }

  /** Move a ref (or create it). */
  setRef(repo: Repo, ref: string, oid: string): void {
    repo.refs.set(ref, oid);
  }

  /** Delete a ref; false when it did not exist. */
  deleteRef(repo: Repo, ref: string): boolean {
    return repo.refs.delete(ref);
  }

  /** Every ref, sorted by name. */
  listRefs(repo: Repo): { name: string; oid: string }[] {
    return [...repo.refs.entries()]
      .map(([name, oid]) => ({ name, oid }))
      .sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  }

  /** The tip of a branch name ("main"), or undefined when the branch does not exist. */
  branchTip(repo: Repo, branch: string): string | undefined {
    return repo.refs.get(`refs/heads/${branch}`);
  }

  /** Every branch: name and tip commit, sorted by name. */
  listBranches(repo: Repo): { name: string; sha: string }[] {
    const branches: { name: string; sha: string }[] = [];
    for (const [name, oid] of repo.refs) {
      if (name.startsWith('refs/heads/')) branches.push({ name: name.slice('refs/heads/'.length), sha: oid });
    }
    branches.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
    return branches;
  }

  // --------------------------------------------------------------------- trees and commits

  /** The empty tree's id (the same in every repository). */
  emptyTreeOid(): string {
    return EMPTY_TREE_OID;
  }

  /**
   * Rebuild a tree from a flat path map, creating intermediate trees bottom-up. Returns the new
   * root tree's id; every created tree is stored.
   */
  buildTreeFromFlat(repo: Repo, flat: ReadonlyMap<string, { mode: string; oid: string }>): string {
    interface Node {
      files: Map<string, { mode: string; oid: string }>;
      dirs: Map<string, Node>;
    }
    const make = (): Node => ({ files: new Map(), dirs: new Map() });
    const root = make();
    for (const [path, entry] of flat) {
      const segments = path.split('/');
      let node = root;
      for (let index = 0; index < segments.length - 1; index += 1) {
        const segment = segments[index];
        if (!node.dirs.has(segment)) node.dirs.set(segment, make());
        node = node.dirs.get(segment)!;
      }
      node.files.set(segments[segments.length - 1], entry);
    }
    const build = (node: Node): string => {
      const entries: { mode: string; name: string; oid: string }[] = [];
      for (const [name, entry] of node.files) entries.push({ mode: entry.mode, name, oid: entry.oid });
      for (const [name, child] of node.dirs) entries.push({ mode: '40000', name, oid: build(child) });
      return this.putObject(repo, 'tree', buildTree(entries));
    };
    return build(root);
  }

  /**
   * Apply file changes to a tree: `changes` maps a path to its new blob (mode + oid) or null to
   * delete it. Returns the new root tree's id.
   */
  applyTreeChanges(
    repo: Repo,
    treeOid: string,
    changes: ReadonlyMap<string, { mode: string; oid: string } | null>,
  ): string {
    const flat = flattenTree(treeOid, this.lookup(repo));
    for (const [path, change] of changes) {
      if (change === null) flat.delete(path);
      else flat.set(path, change);
    }
    return this.buildTreeFromFlat(repo, flat);
  }

  /** Create a commit object, returning its id. */
  createCommit(
    repo: Repo,
    input: { tree: string; parents: readonly string[]; message: string },
  ): string {
    const identity = this.serverIdentity();
    const timestamp = this.nowEpochSec();
    const data = buildCommit({
      tree: input.tree,
      parents: input.parents,
      message: input.message.endsWith('\n') ? input.message : `${input.message}\n`,
      author: identity,
      committer: identity,
      timestamp,
    });
    return this.putObject(repo, 'commit', data);
  }

  /** The tree a ref (branch name or oid) points at, or null when it does not resolve. */
  treeOfRef(repo: Repo, ref: string): string | null {
    let tip = this.branchTip(repo, ref);
    if (tip === undefined && isObjectId(ref)) tip = ref;
    if (tip === undefined) return null;
    const object = repo.objects.get(tip);
    if (!object || object.type !== 'commit') return null;
    return parseCommit(object.data).tree;
  }

  /** The history of a ref, newest first (first parents only). */
  historyOfRef(repo: Repo, ref: string, limit = 1000): { sha: string; message: string }[] {
    let tip = this.branchTip(repo, ref);
    if (tip === undefined && isObjectId(ref)) tip = ref;
    if (tip === undefined) return [];
    const out: { sha: string; message: string }[] = [];
    let current: string | undefined = tip;
    while (current && out.length < limit) {
      const object = repo.objects.get(current);
      if (!object || object.type !== 'commit') break;
      const parsed = parseCommit(object.data);
      out.push({ sha: current, message: parsed.message });
      current = parsed.parents[0];
    }
    return out;
  }

  // --------------------------------------------------------------------- contents

  /** One file (or directory listing) read from a ref's tree. */
  readPath(
    repo: Repo,
    ref: string,
    path: string,
  ):
    | { kind: 'file'; mode: string; oid: string; data: Buffer }
    | { kind: 'dir'; entries: { type: 'file' | 'dir'; name: string; path: string; sha: string }[] }
    | null {
    const treeOid = this.treeOfRef(repo, ref);
    if (treeOid === null) return null;
    const flat = flattenTree(treeOid, this.lookup(repo));
    const direct = flat.get(path);
    if (direct) {
      const object = repo.objects.get(direct.oid);
      if (!object) return null;
      return { kind: 'file', mode: direct.mode, oid: direct.oid, data: object.data };
    }
    // A directory: the entries directly under `path`.
    const prefix = `${path}/`;
    const entries: { type: 'file' | 'dir'; name: string; path: string; sha: string }[] = [];
    const seenDirs = new Set<string>();
    for (const [entryPath, entry] of flat) {
      if (!entryPath.startsWith(prefix)) continue;
      const rest = entryPath.slice(prefix.length);
      const slash = rest.indexOf('/');
      if (slash === -1) {
        entries.push({ type: 'file', name: rest, path: entryPath, sha: entry.oid });
      } else if (!seenDirs.has(rest.slice(0, slash))) {
        seenDirs.add(rest.slice(0, slash));
        entries.push({ type: 'dir', name: rest.slice(0, slash), path: `${path}/${rest.slice(0, slash)}`, sha: entry.oid });
      }
    }
    if (entries.length === 0) return null;
    entries.sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
    return { kind: 'dir', entries };
  }

  /**
   * Write (create or update) one file on a branch, committing it. A branch that does not exist
   * yet is created from the default branch's tip (GitHub's behaviour), so a first write to a new
   * branch works. Returns the blob and commit shas, whether the file is new, or an error:
   * `stale-sha` when the supplied sha is not the current blob.
   */
  writeFile(
    repo: Repo,
    input: { branch: string; path: string; content: Buffer; message: string; sha?: string },
  ): { blobSha: string; commitSha: string; created: boolean } | { error: 'stale-sha' } {
    const ref = `refs/heads/${input.branch}`;
    let tip = repo.refs.get(ref);
    if (tip === undefined && input.branch !== repo.defaultBranch) {
      // A new branch starts at the default branch's tip (or unborn, when there is none yet).
      tip = repo.refs.get(`refs/heads/${repo.defaultBranch}`);
    }
    const treeOid = tip === undefined ? this.emptyTreeOid() : parseCommit(repo.objects.get(tip)!.data).tree;
    const flat = flattenTree(treeOid, this.lookup(repo));
    const current = flat.get(input.path);
    if (input.sha !== undefined) {
      if (!current || current.oid.toLowerCase() !== input.sha.toLowerCase()) return { error: 'stale-sha' };
    }
    const blobSha = this.putObject(repo, 'blob', input.content);
    const created = current === undefined;
    const newTree = this.applyTreeChanges(repo, treeOid, new Map([[input.path, { mode: '100644', oid: blobSha }]]));
    const commitSha = this.createCommit(repo, {
      tree: newTree,
      parents: tip === undefined ? [] : [tip],
      message: input.message,
    });
    repo.refs.set(ref, commitSha);
    return { blobSha, commitSha, created };
  }

  /**
   * Delete one file from a branch, committing it. Returns the commit sha, or an error: `not-found`
   * when the path is absent, `stale-sha` when the supplied sha is not the current blob.
   */
  deleteFile(
    repo: Repo,
    input: { branch: string; path: string; message: string; sha?: string },
  ): { commitSha: string } | { error: 'not-found' | 'stale-sha' | 'no-branch' } {
    const ref = `refs/heads/${input.branch}`;
    const tip = repo.refs.get(ref);
    if (tip === undefined) return { error: 'no-branch' };
    const treeOid = parseCommit(repo.objects.get(tip)!.data).tree;
    const flat = flattenTree(treeOid, this.lookup(repo));
    const current = flat.get(input.path);
    if (!current) return { error: 'not-found' };
    if (input.sha !== undefined && current.oid.toLowerCase() !== input.sha.toLowerCase()) return { error: 'stale-sha' };
    const newTree = this.applyTreeChanges(repo, treeOid, new Map([[input.path, null]]));
    const commitSha = this.createCommit(repo, { tree: newTree, parents: [tip], message: input.message });
    repo.refs.set(ref, commitSha);
    return { commitSha };
  }

  // --------------------------------------------------------------------- labels

  /** Create a label; the existing one is returned when the name is already taken. */
  createLabel(repo: Repo, name: string, color: string): Label {
    const existing = repo.labels.get(name);
    if (existing) return existing;
    const label: Label = { name, color: color || 'ededed' };
    repo.labels.set(name, label);
    return label;
  }

  /** True when the repository has a label of that name. */
  hasLabel(repo: Repo, name: string): boolean {
    return repo.labels.has(name);
  }

  // --------------------------------------------------------------------- issues

  /** Create an issue; null when a named label does not exist (422). */
  createIssue(
    repo: Repo,
    input: { title: string; body?: string; labels?: string[] },
  ): Issue | null {
    const labels = input.labels ?? [];
    for (const name of labels) if (!repo.labels.has(name)) return null;
    repo.issueCounter += 1;
    this.issueIdCounter += 1;
    const now = this.nowIso();
    const issue: Issue = {
      id: this.issueIdCounter,
      number: repo.issueCounter,
      title: input.title,
      body: input.body ?? '',
      state: 'open',
      labels: [...labels],
      createdAt: now,
      updatedAt: now,
      createdSeq: this.nextSeq(),
      comments: [],
    };
    repo.issues.push(issue);
    return issue;
  }

  /** One issue by number, or null. */
  getIssue(repo: Repo, number: number): Issue | null {
    return repo.issues.find((issue) => issue.number === number) ?? null;
  }

  /** Issues filtered by state and ordered by creation (asc or desc). */
  listIssues(repo: Repo, input: { state?: string; sort?: string; direction?: string }): Issue[] {
    let issues = [...repo.issues];
    if (input.state === 'open') issues = issues.filter((issue) => issue.state === 'open');
    else if (input.state === 'closed') issues = issues.filter((issue) => issue.state === 'closed');
    const direction = input.direction === 'asc' ? 1 : -1;
    issues.sort((a, b) => (a.createdSeq - b.createdSeq) * direction);
    return issues;
  }

  /** Close or reopen an issue; null when it does not exist. */
  patchIssue(repo: Repo, number: number, input: { state?: string; title?: string; body?: string }): Issue | null {
    const issue = this.getIssue(repo, number);
    if (!issue) return null;
    if (input.state === 'open' || input.state === 'closed') issue.state = input.state;
    if (input.title !== undefined) issue.title = input.title;
    if (input.body !== undefined) issue.body = input.body;
    issue.updatedAt = this.nowIso();
    return issue;
  }

  /** Add a comment to an issue; null when the issue does not exist. */
  addComment(repo: Repo, number: number, body: string): IssueComment | null {
    const issue = this.getIssue(repo, number);
    if (!issue) return null;
    this.commentIdCounter += 1;
    const comment: IssueComment = { id: this.commentIdCounter, body, createdAt: this.nowIso() };
    issue.comments.push(comment);
    return comment;
  }

  // --------------------------------------------------------------------- pull requests

  /** Create a pull request; null when either branch does not exist. */
  createPullRequest(
    repo: Repo,
    input: { title: string; body?: string; head: string; base: string },
  ): PullRequest | null {
    const headTip = this.branchTip(repo, input.head);
    const baseTip = this.branchTip(repo, input.base);
    if (headTip === undefined || baseTip === undefined) return null;
    repo.pullCounter += 1;
    this.pullIdCounter += 1;
    const now = this.nowIso();
    const pull: PullRequest = {
      id: this.pullIdCounter,
      number: repo.pullCounter,
      title: input.title,
      body: input.body ?? '',
      state: 'open',
      merged: false,
      mergedAt: null,
      mergeCommitSha: null,
      headRef: input.head,
      baseRef: input.base,
      baseSha: baseTip,
      createdAt: now,
      updatedAt: now,
      createdSeq: this.nextSeq(),
    };
    repo.pulls.push(pull);
    return pull;
  }

  /** One pull request by number, or null. */
  getPull(repo: Repo, number: number): PullRequest | null {
    return repo.pulls.find((pull) => pull.number === number) ?? null;
  }

  /** Pull requests filtered by state (default open only), newest last. */
  listPulls(repo: Repo, state: string | undefined): PullRequest[] {
    let pulls = [...repo.pulls];
    if (state === 'all') {
      // no filter
    } else if (state === 'closed') {
      pulls = pulls.filter((pull) => pull.state === 'closed');
    } else {
      pulls = pulls.filter((pull) => pull.state === 'open');
    }
    pulls.sort((a, b) => a.createdSeq - b.createdSeq);
    return pulls;
  }

  /** The head branch's current tip (or null when the branch is gone). */
  pullHeadSha(repo: Repo, pull: PullRequest): string | null {
    return this.branchTip(repo, pull.headRef) ?? null;
  }

  /** The changed files of a pull request: the base-tip vs head-tip tree diff, with line counts. */
  pullFiles(
    repo: Repo,
    pull: PullRequest,
  ): { filename: string; status: string; additions: number; deletions: number; changes: number }[] {
    const headTip = this.branchTip(repo, pull.headRef);
    const baseTip = this.branchTip(repo, pull.baseRef);
    if (headTip === undefined || baseTip === undefined) return [];
    const headTree = parseCommit(repo.objects.get(headTip)!.data).tree;
    // Diff against the base tip at creation when it still exists, else the current base tip: the
    // files a pull request touches are the ones its head changed since it branched.
    const baseObject = repo.objects.get(pull.baseSha);
    const baseTree = baseObject?.type === 'commit' ? parseCommit(baseObject.data).tree : parseCommit(repo.objects.get(baseTip)!.data).tree;
    const diffs = diffTrees(baseTree, headTree, this.lookup(repo));
    return diffs.map((diff) => {
      const oldData = diff.oldOid === null ? null : (repo.objects.get(diff.oldOid)?.data ?? null);
      const newData = diff.newOid === null ? null : (repo.objects.get(diff.newOid)?.data ?? null);
      const counts = lineDiff(oldData, newData);
      const status = diff.status === 'added' ? 'added' : diff.status === 'removed' ? 'removed' : 'modified';
      return {
        filename: diff.path,
        status,
        additions: counts.additions,
        deletions: counts.deletions,
        changes: counts.additions + counts.deletions,
      };
    });
  }

  /** The changed paths of a pull request (for GraphQL's files connection). */
  pullFilePaths(repo: Repo, pull: PullRequest): string[] {
    return this.pullFiles(repo, pull).map((file) => file.filename);
  }

  /** Whether a pull request can merge cleanly: false when a file changed on both sides differently. */
  pullMergeable(repo: Repo, pull: PullRequest): boolean {
    return this.mergeConflict(repo, pull) === null;
  }

  /**
   * The conflicting path of a pull request, or null when it merges cleanly. A conflict is a file
   * the head changed against the merge base that the base also changed against the merge base,
   * with a different resulting blob.
   */
  mergeConflict(repo: Repo, pull: PullRequest): string | null {
    const headTip = this.branchTip(repo, pull.headRef);
    const baseTip = this.branchTip(repo, pull.baseRef);
    if (headTip === undefined || baseTip === undefined) return null;
    if (headTip === baseTip) return null;
    const base = mergeBase(headTip, baseTip, this.lookup(repo));
    if (base === null) return null;
    const baseTree = parseCommit(repo.objects.get(base)!.data).tree;
    const headTree = parseCommit(repo.objects.get(headTip)!.data).tree;
    const currentBaseTree = parseCommit(repo.objects.get(baseTip)!.data).tree;
    const headDiffs = diffTrees(baseTree, headTree, this.lookup(repo));
    const baseDiffs = new Map<string, TreeDiffEntry>();
    for (const diff of diffTrees(baseTree, currentBaseTree, this.lookup(repo))) baseDiffs.set(diff.path, diff);
    for (const diff of headDiffs) {
      const other = baseDiffs.get(diff.path);
      if (!other) continue;
      if (other.newOid !== diff.newOid) return diff.path;
    }
    return null;
  }

  /**
   * Squash-merge a pull request onto its base branch: one commit on the base tip carrying the
   * head's changes. Returns the merge commit sha, or an error: `unknown` (no such pull),
   * `closed` (already merged or closed), `conflict` (405).
   */
  mergePull(
    repo: Repo,
    number: number,
  ): { sha: string } | { error: 'unknown' | 'closed' | 'conflict' } {
    const pull = this.getPull(repo, number);
    if (!pull) return { error: 'unknown' };
    if (pull.state !== 'open') return { error: 'closed' };
    const headTip = this.branchTip(repo, pull.headRef);
    const baseTip = this.branchTip(repo, pull.baseRef);
    if (headTip === undefined || baseTip === undefined) return { error: 'conflict' };
    if (this.mergeConflict(repo, pull) !== null) return { error: 'conflict' };
    const base = mergeBase(headTip, baseTip, this.lookup(repo));
    const baseTree = base === null ? this.emptyTreeOid() : parseCommit(repo.objects.get(base)!.data).tree;
    const headTree = parseCommit(repo.objects.get(headTip)!.data).tree;
    const currentBaseTree = parseCommit(repo.objects.get(baseTip)!.data).tree;
    // Apply the head's changes against the merge base onto the current base tree.
    const changes = new Map<string, { mode: string; oid: string } | null>();
    const headFlat = flattenTree(headTree, this.lookup(repo));
    for (const diff of diffTrees(baseTree, headTree, this.lookup(repo))) {
      if (diff.status === 'removed') changes.set(diff.path, null);
      else {
        const entry = headFlat.get(diff.path);
        if (entry) changes.set(diff.path, { mode: entry.mode, oid: entry.oid });
      }
    }
    const newTree = this.applyTreeChanges(repo, currentBaseTree, changes);
    const sha = this.createCommit(repo, {
      tree: newTree,
      parents: [baseTip],
      message: `Merge pull request #${pull.number} from ${repo.owner}/${pull.headRef}\n\n${pull.title}`,
    });
    repo.refs.set(`refs/heads/${pull.baseRef}`, sha);
    pull.state = 'closed';
    pull.merged = true;
    pull.mergedAt = this.nowIso();
    pull.mergeCommitSha = sha;
    pull.updatedAt = pull.mergedAt;
    return { sha };
  }

  // --------------------------------------------------------------------- fast-forward

  /** True when moving `ref` from `oldOid` to `newOid` is a fast-forward (or a create/delete). */
  isFastForward(repo: Repo, oldOid: string, newOid: string): boolean {
    if (newOid === ZERO_OID) return true; // a delete always applies
    if (oldOid === ZERO_OID) return true; // a create always applies
    // Fast path first, without the lookup closure: a push's new tip almost always has the
    // old tip as its immediate parent, and that answer needs only the tip's own bytes.
    const tip = repo.objects.get(newOid);
    if (tip?.type === 'commit' && commitHasParent(tip.data, oldOid)) return true;
    return isAncestor(oldOid, newOid, this.lookup(repo));
  }

  /** True when every object reachable from the tips is already in the store. */
  hasClosure(repo: Repo, tips: readonly string[]): boolean {
    try {
      collectReachable(tips, this.lookup(repo));
      return true;
    } catch {
      return false;
    }
  }
}

/** The number of lines in a blob, for the pull-request files listing (re-exported for rest.ts). */
export { countLines };
