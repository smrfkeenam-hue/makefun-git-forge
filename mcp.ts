// MCP: JSON-RPC 2.0 over Streamable HTTP (plain JSON answers, no SSE stream). The modern era
// only: the client probes server/discover, negotiates revision 2026-07-28, and calls tools/list
// and tools/call. Every tool acts on the same store as REST, with the same stable ids.
//
// Wire rules: auth (Bearer $MAKEFUN_SHOP_TOKEN) is decided before anything else — a request
// without it is HTTP 401, even for a tool that does not exist. A body that is not JSON is
// HTTP 400 with JSON-RPC -32700. An unknown tool is -32602 (never HTTP 500). A call missing a
// required argument is -32602. Every request answers well inside the client's 3 s timeout.

import type { Store } from './store.ts';
import type { RestReply } from './rest.ts';

/** One tool's declaration: name, description and input schema (fixed, byte-identical per call). */
interface ToolDef {
  readonly name: string;
  readonly description: string;
  readonly inputSchema: {
    readonly type: 'object';
    readonly properties: Record<string, { type: string; description: string }>;
    readonly required: string[];
  };
}

const stringProp = (description: string): { type: string; description: string } => ({ type: 'string', description });

/** The six tools, in a fixed order. `required` holds only properties the suite fills. */
const TOOLS: readonly ToolDef[] = [
  {
    name: 'create_repo',
    description: 'Create a repository owned by the authenticated user.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: stringProp('The login that will own the repository.'),
        name: stringProp('The repository name.'),
        description: stringProp('A short description of the repository.'),
        private: { type: 'boolean', description: 'Whether the repository is private.' },
      },
      required: ['name'],
    },
  },
  {
    name: 'get_repo',
    description: 'Read one repository by owner and name.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: stringProp('The repository owner login.'),
        repo: stringProp('The repository name.'),
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: stringProp('The repository owner login.'),
        repo: stringProp('The repository name.'),
        title: stringProp('The issue title.'),
        body: stringProp('The issue body.'),
      },
      required: ['owner', 'repo', 'title'],
    },
  },
  {
    name: 'list_issues',
    description: 'List the issues of a repository, optionally filtered by state.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: stringProp('The repository owner login.'),
        repo: stringProp('The repository name.'),
        state: stringProp('Issue state to list: open, closed, or all.'),
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_pull_request',
    description: 'Open a pull request from a head branch onto a base branch.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: stringProp('The repository owner login.'),
        repo: stringProp('The repository name.'),
        title: stringProp('The pull request title.'),
        body: stringProp('The pull request body.'),
        head: stringProp('The head branch name.'),
        base: stringProp('The base branch name.'),
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Squash-merge a pull request onto its base branch.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: stringProp('The repository owner login.'),
        repo: stringProp('The repository name.'),
        pull_number: { type: 'number', description: 'The pull request number.' },
      },
      required: ['owner', 'repo', 'pull_number'],
    },
  },
];

const SERVER_INFO = { name: 'makefun-forge', version: '1.0.0' };

type JsonRpcId = string | number | null;

function rpcResult(id: JsonRpcId, result: unknown): RestReply {
  return { status: 200, json: { jsonrpc: '2.0', id, result } };
}

function rpcError(id: JsonRpcId, code: number, message: string, status = 200): RestReply {
  return { status, json: { jsonrpc: '2.0', id, error: { code, message } } };
}

/** A tool result: a text block carrying the JSON object, and the same object structured. */
function toolResult(id: JsonRpcId, value: unknown): RestReply {
  const text = JSON.stringify(value);
  return rpcResult(id, {
    content: [{ type: 'text', text }],
    structuredContent: value,
  });
}

function toolError(id: JsonRpcId, message: string): RestReply {
  return rpcResult(id, {
    content: [{ type: 'text', text: message }],
    structuredContent: { error: message },
    isError: true,
  });
}

function asRecord(value: unknown): Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
    ? (value as Record<string, unknown>)
    : {};
}

/** The required arguments a tool call is missing (empty when the call is well formed). */
function missingArgs(tool: ToolDef, args: Record<string, unknown>): string[] {
  return tool.inputSchema.required.filter((name) => {
    const value = args[name];
    return value === undefined || value === null || value === '';
  });
}

/** Execute one tool call against the store. */
function callTool(store: Store, id: JsonRpcId, name: string, args: Record<string, unknown>): RestReply {
  const tool = TOOLS.find((candidate) => candidate.name === name);
  if (!tool) return rpcError(id, -32602, `unknown tool ${name}`);
  const missing = missingArgs(tool, args);
  if (missing.length > 0) return rpcError(id, -32602, `missing required argument(s): ${missing.join(', ')}`);

  const owner = typeof args.owner === 'string' ? args.owner : store.viewerLogin();
  const repoName = typeof args.repo === 'string' ? args.repo : typeof args.name === 'string' ? args.name : '';

  switch (name) {
    case 'create_repo': {
      const repo = store.createRepo(repoName, {
        private: typeof args.private === 'boolean' ? args.private : undefined,
        description: typeof args.description === 'string' ? args.description : undefined,
      });
      if (!repo) return toolError(id, `repository ${repoName} already exists`);
      return toolResult(id, {
        id: repo.id,
        name: repo.name,
        full_name: `${owner}/${repo.name}`,
        owner: { login: owner },
        private: repo.private,
        description: repo.description,
        default_branch: repo.defaultBranch,
      });
    }
    case 'get_repo': {
      const repo = store.getRepo(owner, repoName);
      if (!repo) return toolError(id, `repository ${owner}/${repoName} not found`);
      return toolResult(id, {
        id: repo.id,
        name: repo.name,
        full_name: `${owner}/${repo.name}`,
        owner: { login: owner },
        private: repo.private,
        description: repo.description,
        default_branch: repo.defaultBranch,
      });
    }
    case 'create_issue': {
      const repo = store.getRepo(owner, repoName);
      if (!repo) return toolError(id, `repository ${owner}/${repoName} not found`);
      const issue = store.createIssue(repo, {
        title: String(args.title ?? ''),
        body: typeof args.body === 'string' ? args.body : '',
      });
      if (!issue) return toolError(id, 'could not create the issue');
      return toolResult(id, {
        id: issue.id,
        number: issue.number,
        title: issue.title,
        body: issue.body,
        state: issue.state,
        full_name: `${owner}/${repoName}`,
      });
    }
    case 'list_issues': {
      const repo = store.getRepo(owner, repoName);
      if (!repo) return toolError(id, `repository ${owner}/${repoName} not found`);
      const state = typeof args.state === 'string' ? args.state : 'all';
      const issues = store.listIssues(repo, { state }).map((issue) => ({
        id: issue.id,
        number: issue.number,
        title: issue.title,
        body: issue.body,
        state: issue.state,
      }));
      return toolResult(id, { issues, total_count: issues.length });
    }
    case 'create_pull_request': {
      const repo = store.getRepo(owner, repoName);
      if (!repo) return toolError(id, `repository ${owner}/${repoName} not found`);
      const pull = store.createPullRequest(repo, {
        title: String(args.title ?? ''),
        body: typeof args.body === 'string' ? args.body : '',
        head: String(args.head ?? ''),
        base: typeof args.base === 'string' ? args.base : repo.defaultBranch,
      });
      if (!pull) return toolError(id, 'head or base branch does not exist');
      return toolResult(id, {
        id: pull.id,
        number: pull.number,
        pull_number: pull.number,
        title: pull.title,
        body: pull.body,
        state: pull.state,
        head: pull.headRef,
        base: pull.baseRef,
        full_name: `${owner}/${repoName}`,
      });
    }
    case 'merge_pull_request': {
      const repo = store.getRepo(owner, repoName);
      if (!repo) return toolError(id, `repository ${owner}/${repoName} not found`);
      const pullNumber = typeof args.pull_number === 'number' ? args.pull_number : parseInt(String(args.pull_number ?? ''), 10);
      if (!Number.isFinite(pullNumber)) return rpcError(id, -32602, 'pull_number must be a number');
      const merged = store.mergePull(repo, pullNumber);
      if ('error' in merged) return toolError(id, `could not merge the pull request: ${merged.error}`);
      return toolResult(id, { number: pullNumber, pull_number: pullNumber, merged: true, sha: merged.sha });
    }
    default:
      return rpcError(id, -32602, `unknown tool ${name}`);
  }
}

/**
 * Handle POST /mcp. Auth is enforced by the router before this runs. A body that is not JSON is
 * HTTP 400 with -32700; a notification (no id) is acknowledged with an empty 200.
 */
export function handleMcp(store: Store, body: Buffer): RestReply {
  let parsed: unknown;
  try {
    parsed = JSON.parse(body.toString('utf8'));
  } catch {
    return rpcError(null, -32700, 'Parse error', 400);
  }
  const request = asRecord(parsed);
  const id = (typeof request.id === 'string' || typeof request.id === 'number' ? request.id : null) as JsonRpcId;
  const method = typeof request.method === 'string' ? request.method : '';
  const params = asRecord(request.params);

  // A notification carries no id and expects no answer.
  if (request.id === undefined) {
    return { status: 200, json: {} };
  }

  if (method === 'server/discover') {
    return rpcResult(id, {
      supportedVersions: ['2026-07-28'],
      capabilities: { tools: {} },
      serverInfo: SERVER_INFO,
      _meta: { 'io.modelcontextprotocol/serverInfo': SERVER_INFO },
    });
  }
  if (method === 'initialize') {
    const offered = typeof params.protocolVersion === 'string' ? params.protocolVersion : '2026-07-28';
    return rpcResult(id, {
      protocolVersion: offered,
      capabilities: { tools: {} },
      serverInfo: SERVER_INFO,
    });
  }
  if (method === 'tools/list') {
    return rpcResult(id, { tools: TOOLS });
  }
  if (method === 'tools/call') {
    const toolName = typeof params.name === 'string' ? params.name : '';
    const args = asRecord(params.arguments);
    return callTool(store, id, toolName, args);
  }
  if (method === 'ping') {
    return rpcResult(id, {});
  }
  return rpcError(id, -32601, `method not found: ${method}`);
}
