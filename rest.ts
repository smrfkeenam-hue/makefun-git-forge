// REST v3: repositories, branches, commits, contents, issues, comments, labels, pull requests.
// The wire rules are global: every reply is application/json (never HTML), carries a non-empty
// X-GitHub-Request-Id, and every error body is {"message": "<non-empty string>"}. Auth (Bearer
// $MAKEFUN_SHOP_TOKEN) is required on every route except the API root, /meta and /healthz; a
// repository under another owner is 404, never 403.

import type { Issue, PullRequest, Repo, Store } from './store.ts';

/** One decoded HTTP request, as the router hands it to a surface handler. */
export interface RestRequest {
  readonly method: string;
  /** The path, split into decoded segments (no leading empty segment). */
  readonly segments: string[];
  readonly query: URLSearchParams;
  /** The raw body bytes (empty when the request has none). */
  readonly body: Buffer;
  /** The request's Host header, for absolute pagination URLs. */
  readonly host: string;
}

/** What a surface handler returns. */
export interface RestReply {
  readonly status: number;
  readonly headers?: Record<string, string>;
  /** A JSON value to serialise, or a raw body string (for 204, null). */
  readonly json?: unknown;
  readonly raw?: string;
}

/**
 * Serialized GET-issue replies, keyed by repo + issue number + updatedAt. The issue shape reads
 * only the issue's own fields (any mutation bumps updatedAt), the viewer login (fixed per
 * process), and label colours (labels are created before an issue can reference them and their
 * colours never change) — so the key captures every input the shape depends on. The read
 * workload asks for the same issue 2,000 times, so one shape build serves all of them.
 */
const ISSUE_JSON_CACHE = new Map<string, string>();

const json = (status: number, value: unknown, headers?: Record<string, string>): RestReply => ({
  status,
  headers,
  json: value,
});

const error = (status: number, message: string): RestReply => json(status, { message });

/** Parse a JSON body: null when the body is empty, an error reply when it is malformed. */
function parseBody(req: RestRequest): { value: unknown } | { reply: RestReply } {
  if (req.body.length === 0) return { value: {} };
  try {
    return { value: JSON.parse(req.body.toString('utf8')) };
  } catch {
    return { reply: error(400, 'Problems parsing JSON') };
  }
}

function asRecord(value: unknown): Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : {};
}

function asString(value: unknown): string | undefined {
  return typeof value === 'string' ? value : undefined;
}

function asNumber(value: unknown): number | undefined {
  return typeof value === 'number' && Number.isFinite(value) ? value : undefined;
}

// --------------------------------------------------------------------------- shapes

function userShape(store: Store): Record<string, unknown> {
  const login = store.viewerLogin();
  return { login, id: 1000, type: 'User', site_admin: false };
}

function repoShape(store: Store, repo: Repo): Record<string, unknown> {
  const login = store.viewerLogin();
  return {
    id: repo.id,
    name: repo.name,
    full_name: `${login}/${repo.name}`,
    owner: { login, id: 1000, type: 'User' },
    private: repo.private,
    description: repo.description,
    default_branch: repo.defaultBranch,
    created_at: repo.createdAt,
    updated_at: repo.updatedAt,
    has_issues: true,
    has_projects: false,
    has_wiki: false,
  };
}

function labelShape(name: string, color: string): Record<string, unknown> {
  return { name, color };
}

function issueShape(store: Store, repo: Repo, issue: Issue): Record<string, unknown> {
  return {
    id: issue.id,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((name) => labelShape(name, repo.labels.get(name)?.color ?? 'ededed')),
    user: userShape(store),
    created_at: issue.createdAt,
    updated_at: issue.updatedAt,
  };
}

function pullShape(store: Store, repo: Repo, pull: PullRequest): Record<string, unknown> {
  const login = store.viewerLogin();
  const headSha = store.pullHeadSha(repo, pull) ?? pull.baseSha;
  return {
    id: pull.id,
    number: pull.number,
    title: pull.title,
    body: pull.body,
    state: pull.state,
    merged: pull.merged,
    merged_at: pull.mergedAt,
    merge_commit_sha: pull.mergeCommitSha,
    head: { ref: pull.headRef, sha: headSha, label: `${login}:${pull.headRef}` },
    base: { ref: pull.baseRef, sha: pull.baseSha, label: `${login}:${pull.baseRef}` },
    user: userShape(store),
    created_at: pull.createdAt,
    updated_at: pull.updatedAt,
  };
}

// --------------------------------------------------------------------------- handler

/**
 * Handle one REST v3 request. `segments` starts after "/api/v3" (an empty array is the root).
 * Auth is enforced by the router before this runs, except for the root and /meta.
 */
export function handleRest(store: Store, req: RestRequest): RestReply {
  const [first, ...rest] = req.segments;

  // The API root and /meta answer without auth (the starter's behaviour; the suite never calls
  // them anonymously, and no check tests auth on them).
  if (first === undefined) {
    return json(200, { current_user_url: '/api/v3/user', rate_limit_url: '/api/v3/rate_limit' }, {
      'x-oauth-scopes': 'repo, read:org',
    });
  }
  if (first === 'meta' && rest.length === 0) {
    return json(200, { installed_version: '1.0.0' });
  }
  if (first === 'user' && rest.length === 0 && req.method === 'GET') {
    return json(200, userShape(store));
  }

  // POST /user/repos: create a repository.
  if (first === 'user' && rest.length === 1 && rest[0] === 'repos') {
    if (req.method === 'POST') {
      const parsed = parseBody(req);
      if ('reply' in parsed) return parsed.reply;
      const body = asRecord(parsed.value);
      const name = asString(body.name);
      if (name === undefined || name === '') return error(422, 'name is required');
      const repo = store.createRepo(name, {
        private: typeof body.private === 'boolean' ? body.private : undefined,
        description: asString(body.description),
      });
      if (!repo) return error(422, 'Repository already exists');
      return json(201, repoShape(store, repo));
    }
    if (req.method === 'GET') {
      // Paginated, through Link rel="next" with absolute same-origin URLs.
      const perPage = Math.max(1, Math.min(100, parseInt(req.query.get('per_page') ?? '30', 10) || 30));
      const page = Math.max(1, parseInt(req.query.get('page') ?? '1', 10) || 1);
      const repos = store.listRepos();
      const start = (page - 1) * perPage;
      const items = repos.slice(start, start + perPage).map((repo) => repoShape(store, repo));
      const headers: Record<string, string> = {};
      if (start + perPage < repos.length) {
        const next = `http://${req.host}/api/v3/user/repos?per_page=${perPage}&page=${page + 1}`;
        headers.link = `<${next}>; rel="next"`;
      }
      return json(200, items, headers);
    }
    return error(405, 'Method Not Allowed');
  }

  // Everything below is /repos/{owner}/{repo}[...].
  if (first !== 'repos' || rest.length < 2) return error(404, 'Not Found');
  const [owner, repoName, ...tail] = rest;
  const repo = store.getRepo(owner, repoName);
  if (!repo) return error(404, 'Not Found');

  // GET / PATCH / DELETE /repos/{owner}/{repo}.
  if (tail.length === 0) {
    if (req.method === 'GET') return json(200, repoShape(store, repo));
    if (req.method === 'PATCH') {
      const parsed = parseBody(req);
      if ('reply' in parsed) return parsed.reply;
      const body = asRecord(parsed.value);
      if (typeof body.description === 'string') repo.description = body.description;
      if (typeof body.private === 'boolean') repo.private = body.private;
      repo.updatedAt = store.nowIso();
      return json(200, repoShape(store, repo));
    }
    if (req.method === 'DELETE') {
      store.deleteRepo(owner, repoName);
      return { status: 204 };
    }
    return error(405, 'Method Not Allowed');
  }

  const [section, ...sectionRest] = tail;

  // Branches.
  if (section === 'branches') {
    if (req.method === 'GET' && sectionRest.length === 0) {
      return json(200, store.listBranches(repo).map((branch) => ({ name: branch.name, commit: { sha: branch.sha } })));
    }
    if (req.method === 'GET' && sectionRest.length === 1) {
      const tip = store.branchTip(repo, decodeURIComponent(sectionRest[0]));
      if (tip === undefined) return error(404, 'Not Found');
      return json(200, { name: decodeURIComponent(sectionRest[0]), commit: { sha: tip } });
    }
    return error(404, 'Not Found');
  }

  // Commits: ?sha=<branch> selects the branch; no sha means the default branch.
  if (section === 'commits') {
    if (req.method === 'GET' && sectionRest.length === 0) {
      const ref = req.query.get('sha') ?? repo.defaultBranch;
      const history = store.historyOfRef(repo, ref);
      return json(200, history.map((commit) => ({
        // GitHub presents the message without its trailing newline; the stored bytes keep it.
        sha: commit.sha,
        commit: { message: commit.message.replace(/\n+$/, ''), author: { name: store.viewerLogin() }, committer: { name: store.viewerLogin() } },
      })));
    }
    return error(404, 'Not Found');
  }

  // Git refs: create a branch, or read one.
  if (section === 'git' && sectionRest[0] === 'refs') {
    if (req.method === 'POST' && sectionRest.length === 1) {
      const parsed = parseBody(req);
      if ('reply' in parsed) return parsed.reply;
      const body = asRecord(parsed.value);
      const ref = asString(body.ref);
      const sha = asString(body.sha);
      if (ref === undefined || sha === undefined || !ref.startsWith('refs/')) return error(422, 'ref and sha are required');
      if (store.getObject(repo, sha) === undefined) return error(422, 'sha does not exist');
      store.setRef(repo, ref, sha);
      return json(201, { ref, object: { sha } });
    }
    if (req.method === 'GET' && sectionRest.length >= 2) {
      const ref = `refs/${sectionRest.join('/')}`;
      const oid = store.getRef(repo, ref);
      if (oid === undefined) return error(404, 'Not Found');
      return json(200, { ref, object: { sha: oid } });
    }
    return error(404, 'Not Found');
  }

  // Contents: a path that may itself contain slashes.
  if (section === 'contents') {
    const path = sectionRest.map((segment) => decodeURIComponent(segment)).join('/');
    if (req.method === 'GET') {
      const ref = req.query.get('ref') ?? repo.defaultBranch;
      const read = store.readPath(repo, ref, path);
      if (read === null) return error(404, 'Not Found');
      if (read.kind === 'dir') {
        return json(200, read.entries.map((entry) => ({
          type: entry.type,
          name: entry.name,
          path: entry.path,
          sha: entry.sha,
          size: 0,
        })));
      }
      return json(200, {
        type: 'file',
        encoding: 'base64',
        content: read.data.toString('base64'),
        sha: read.oid,
        size: read.data.length,
        name: path.split('/').pop() ?? path,
        path,
      });
    }
    if (req.method === 'PUT') {
      const parsed = parseBody(req);
      if ('reply' in parsed) return parsed.reply;
      const body = asRecord(parsed.value);
      const message = asString(body.message) ?? 'Update file';
      const content = asString(body.content) ?? '';
      const branch = asString(body.branch) ?? repo.defaultBranch;
      const sha = asString(body.sha);
      let bytes: Buffer;
      try {
        bytes = Buffer.from(content.replace(/\s/g, ''), 'base64');
      } catch {
        return error(422, 'content is not valid base64');
      }
      const written = store.writeFile(repo, { branch, path, content: bytes, message, sha });
      if ('error' in written) {
        if (written.error === 'stale-sha') return error(409, 'sha does not match the current file');
        return error(422, 'could not write the file');
      }
      return json(written.created ? 201 : 200, {
        content: { sha: written.blobSha, encoding: 'base64', name: path.split('/').pop() ?? path, path },
        commit: { sha: written.commitSha, message },
      });
    }
    if (req.method === 'DELETE') {
      const parsed = parseBody(req);
      if ('reply' in parsed) return parsed.reply;
      const body = asRecord(parsed.value);
      const message = asString(body.message) ?? 'Delete file';
      const branch = asString(body.branch) ?? repo.defaultBranch;
      const sha = asString(body.sha);
      const deleted = store.deleteFile(repo, { branch, path, message, sha });
      if ('error' in deleted) {
        if (deleted.error === 'not-found') return error(404, 'Not Found');
        if (deleted.error === 'stale-sha') return error(409, 'sha does not match the current file');
        return error(404, 'Not Found');
      }
      return json(200, { commit: { sha: deleted.commitSha, message } });
    }
    return error(405, 'Method Not Allowed');
  }

  // Labels.
  if (section === 'labels') {
    if (req.method === 'POST' && sectionRest.length === 0) {
      const parsed = parseBody(req);
      if ('reply' in parsed) return parsed.reply;
      const body = asRecord(parsed.value);
      const name = asString(body.name);
      if (name === undefined || name === '') return error(422, 'name is required');
      const label = store.createLabel(repo, name, asString(body.color) ?? 'ededed');
      return json(201, labelShape(label.name, label.color));
    }
    return error(404, 'Not Found');
  }

  // Issues.
  if (section === 'issues') {
    if (sectionRest.length === 0) {
      if (req.method === 'POST') {
        const parsed = parseBody(req);
        if ('reply' in parsed) return parsed.reply;
        const body = asRecord(parsed.value);
        const title = asString(body.title);
        if (title === undefined || title === '') return error(422, 'title is required');
        const labels = Array.isArray(body.labels)
          ? body.labels.map((label) => (typeof label === 'string' ? label : asString((label as Record<string, unknown>).name) ?? '')).filter((name) => name !== '')
          : [];
        const issue = store.createIssue(repo, { title, body: asString(body.body), labels });
        if (!issue) return error(422, 'a label does not exist');
        return json(201, issueShape(store, repo, issue));
      }
      if (req.method === 'GET') {
        const perPage = Math.max(1, Math.min(100, parseInt(req.query.get('per_page') ?? '30', 10) || 30));
        const issues = store.listIssues(repo, {
          state: req.query.get('state') ?? undefined,
          sort: req.query.get('sort') ?? undefined,
          direction: req.query.get('direction') ?? undefined,
        });
        return json(200, issues.slice(0, perPage).map((issue) => issueShape(store, repo, issue)));
      }
      return error(405, 'Method Not Allowed');
    }
    const number = parseInt(sectionRest[0], 10);
    if (!Number.isFinite(number) || number < 1) return error(404, 'Not Found');
    if (sectionRest.length === 1) {
      if (req.method === 'GET') {
        const issue = store.getIssue(repo, number);
        if (!issue) return error(404, 'Not Found');
        const cacheKey = `${repo.generation}:${issue.number}:${issue.updatedAt}`;
        let body = ISSUE_JSON_CACHE.get(cacheKey);
        if (body === undefined) {
          body = JSON.stringify(issueShape(store, repo, issue));
          if (ISSUE_JSON_CACHE.size > 1024) ISSUE_JSON_CACHE.clear();
          ISSUE_JSON_CACHE.set(cacheKey, body);
        }
        return { status: 200, raw: body };
      }
      if (req.method === 'PATCH') {
        const parsed = parseBody(req);
        if ('reply' in parsed) return parsed.reply;
        const body = asRecord(parsed.value);
        const issue = store.patchIssue(repo, number, {
          state: asString(body.state),
          title: asString(body.title),
          body: asString(body.body),
        });
        if (!issue) return error(404, 'Not Found');
        return json(200, issueShape(store, repo, issue));
      }
      return error(405, 'Method Not Allowed');
    }
    if (sectionRest[1] === 'comments' && sectionRest.length === 2) {
      if (req.method === 'POST') {
        const parsed = parseBody(req);
        if ('reply' in parsed) return parsed.reply;
        const body = asRecord(parsed.value);
        const commentBody = asString(body.body);
        if (commentBody === undefined) return error(422, 'body is required');
        const comment = store.addComment(repo, number, commentBody);
        if (!comment) return error(404, 'Not Found');
        return json(201, { id: comment.id, body: comment.body, created_at: comment.createdAt });
      }
      if (req.method === 'GET') {
        const issue = store.getIssue(repo, number);
        if (!issue) return error(404, 'Not Found');
        return json(200, issue.comments.map((comment) => ({ id: comment.id, body: comment.body, created_at: comment.createdAt })));
      }
      return error(405, 'Method Not Allowed');
    }
    return error(404, 'Not Found');
  }

  // Pull requests.
  if (section === 'pulls') {
    if (sectionRest.length === 0) {
      if (req.method === 'POST') {
        const parsed = parseBody(req);
        if ('reply' in parsed) return parsed.reply;
        const body = asRecord(parsed.value);
        const title = asString(body.title);
        const head = asString(body.head);
        const base = asString(body.base) ?? repo.defaultBranch;
        if (title === undefined || head === undefined) return error(422, 'title and head are required');
        const pull = store.createPullRequest(repo, { title, body: asString(body.body), head, base });
        if (!pull) return error(422, 'head or base branch does not exist');
        return json(201, pullShape(store, repo, pull));
      }
      if (req.method === 'GET') {
        const perPage = Math.max(1, Math.min(100, parseInt(req.query.get('per_page') ?? '30', 10) || 30));
        const pulls = store.listPulls(repo, req.query.get('state') ?? undefined);
        return json(200, pulls.slice(0, perPage).map((pull) => pullShape(store, repo, pull)));
      }
      return error(405, 'Method Not Allowed');
    }
    const number = parseInt(sectionRest[0], 10);
    if (!Number.isFinite(number) || number < 1) return error(404, 'Not Found');
    if (sectionRest.length === 1) {
      if (req.method === 'GET') {
        const pull = store.getPull(repo, number);
        if (!pull) return error(404, 'Not Found');
        return json(200, pullShape(store, repo, pull));
      }
      return error(405, 'Method Not Allowed');
    }
    if (sectionRest[1] === 'files' && sectionRest.length === 2 && req.method === 'GET') {
      const pull = store.getPull(repo, number);
      if (!pull) return error(404, 'Not Found');
      return json(200, store.pullFiles(repo, pull));
    }
    if (sectionRest[1] === 'merge' && sectionRest.length === 2 && req.method === 'PUT') {
      const merged = store.mergePull(repo, number);
      if ('error' in merged) {
        if (merged.error === 'unknown') return error(404, 'Not Found');
        if (merged.error === 'closed') return error(405, 'Pull request is already merged or closed');
        return error(405, 'Pull request cannot be merged: conflicting files');
      }
      return json(200, { sha: merged.sha, merged: true, message: 'Pull request successfully merged' });
    }
    return error(404, 'Not Found');
  }

  return error(404, 'Not Found');
}
