// GraphQL v4: a small executor for the fixed documents the suite sends (the documents `gh` and
// github-mcp-server send). It parses queries and mutations, validates field names against the
// schema, and resolves them against the same store REST uses, so an issue created over GraphQL
// reads back over REST with the same number, title and state (GraphQL OPEN/CLOSED, REST
// open/closed).
//
// Wire contract: POST /api/graphql, one operation {query, variables} or a batch (a JSON array of
// them, answered as an array in order). A GraphQL-level failure is HTTP 200 with an `errors`
// array, never an HTTP error. An unknown repository is data.repository = null with no errors.
// No Authorization header is exactly 401 (enforced by the router).

import type { Issue, PullRequest, Repo, Store } from './store.ts';
import type { RestReply } from './rest.ts';

// --------------------------------------------------------------------------- lexer

type Token =
  | { kind: 'name'; value: string }
  | { kind: 'string'; value: string }
  | { kind: 'int'; value: number }
  | { kind: 'float'; value: number }
  | { kind: 'punct'; value: string }
  | { kind: 'eof' };

function lex(source: string): Token[] {
  const tokens: Token[] = [];
  let i = 0;
  const isNameStart = (ch: string): boolean => /[A-Za-z_]/.test(ch);
  const isNameChar = (ch: string): boolean => /[A-Za-z0-9_]/.test(ch);
  while (i < source.length) {
    const ch = source[i];
    if (ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r' || ch === ',') {
      i += 1;
      continue;
    }
    if (ch === '#') {
      while (i < source.length && source[i] !== '\n') i += 1;
      continue;
    }
    if (ch === '.' && source.slice(i, i + 3) === '...') {
      tokens.push({ kind: 'punct', value: '...' });
      i += 3;
      continue;
    }
    if ('{}()[]:!=$@|&='.includes(ch)) {
      tokens.push({ kind: 'punct', value: ch });
      i += 1;
      continue;
    }
    if (ch === '"') {
      // A string literal (block strings are not used by the suite's documents).
      let value = '';
      i += 1;
      while (i < source.length && source[i] !== '"') {
        if (source[i] === '\\' && i + 1 < source.length) {
          const escaped = source[i + 1];
          value += escaped === 'n' ? '\n' : escaped === 't' ? '\t' : escaped === 'r' ? '\r' : escaped;
          i += 2;
        } else {
          value += source[i];
          i += 1;
        }
      }
      i += 1; // the closing quote
      tokens.push({ kind: 'string', value });
      continue;
    }
    if (ch === '-' || /[0-9]/.test(ch)) {
      let text = '';
      while (i < source.length && /[0-9eE.+-]/.test(source[i])) {
        text += source[i];
        i += 1;
      }
      if (text.includes('.') || text.includes('e') || text.includes('E')) {
        tokens.push({ kind: 'float', value: parseFloat(text) });
      } else {
        tokens.push({ kind: 'int', value: parseInt(text, 10) });
      }
      continue;
    }
    if (isNameStart(ch)) {
      let name = '';
      while (i < source.length && isNameChar(source[i])) {
        name += source[i];
        i += 1;
      }
      tokens.push({ kind: 'name', value: name });
      continue;
    }
    throw new Error(`graphql: unexpected character ${JSON.stringify(ch)}`);
  }
  tokens.push({ kind: 'eof' });
  return tokens;
}

// --------------------------------------------------------------------------- parser

/** A value in an argument: a literal, a list, an input object, an enum, or a variable. */
type Value =
  | string
  | number
  | boolean
  | null
  | Value[]
  | { [key: string]: Value }
  | { kind: 'enum'; value: string }
  | { kind: 'variable'; name: string };

interface FieldNode {
  readonly type: 'field';
  readonly alias: string | null;
  readonly name: string;
  readonly args: Map<string, Value>;
  readonly selections: SelectionNode[];
}

interface FragmentNode {
  readonly type: 'fragment';
  readonly typeCondition: string;
  readonly selections: SelectionNode[];
}

type SelectionNode = FieldNode | FragmentNode;

interface OperationNode {
  readonly kind: 'query' | 'mutation';
  readonly name: string | null;
  readonly selections: SelectionNode[];
}

class Parser {
  private at = 0;
  private readonly tokens: Token[];
  constructor(tokens: Token[]) {
    this.tokens = tokens;
  }

  private peek(): Token {
    return this.tokens[this.at];
  }

  private next(): Token {
    const token = this.tokens[this.at];
    if (token.kind !== 'eof') this.at += 1;
    return token;
  }

  private expectPunct(value: string): void {
    const token = this.next();
    if (token.kind !== 'punct' || token.value !== value) {
      throw new Error(`graphql: expected ${JSON.stringify(value)}`);
    }
  }

  private expectName(): string {
    const token = this.next();
    if (token.kind !== 'name') throw new Error('graphql: expected a name');
    return token.value;
  }

  parseDocument(): OperationNode[] {
    const operations: OperationNode[] = [];
    while (this.peek().kind !== 'eof') {
      operations.push(this.parseOperation());
    }
    return operations;
  }

  private parseOperation(): OperationNode {
    let kind: 'query' | 'mutation' = 'query';
    let name: string | null = null;
    const token = this.peek();
    if (token.kind === 'name' && (token.value === 'query' || token.value === 'mutation')) {
      kind = token.value;
      this.next();
      const maybeName = this.peek();
      if (maybeName.kind === 'name') name = this.next().value as string;
      // Variable definitions: ($name: Type, ...) — the types are not needed to execute.
      if (this.peek().kind === 'punct' && (this.peek() as { value: string }).value === '(') {
        this.skipParens();
      }
    }
    const selections = this.parseSelectionSet();
    return { kind, name, selections };
  }

  /** Skip a balanced (...) group (variable definitions and their defaults). */
  private skipParens(): void {
    let depth = 0;
    while (true) {
      const token = this.next();
      if (token.kind === 'eof') throw new Error('graphql: unbalanced parentheses');
      if (token.kind === 'punct' && token.value === '(') depth += 1;
      if (token.kind === 'punct' && token.value === ')') {
        depth -= 1;
        if (depth === 0) return;
      }
    }
  }

  private parseSelectionSet(): SelectionNode[] {
    this.expectPunct('{');
    const selections: SelectionNode[] = [];
    while (true) {
      const token = this.peek();
      if (token.kind === 'punct' && token.value === '}') {
        this.next();
        return selections;
      }
      if (token.kind === 'eof') throw new Error('graphql: unterminated selection set');
      selections.push(this.parseSelection());
    }
  }

  private parseSelection(): SelectionNode {
    const token = this.peek();
    if (token.kind === 'punct' && token.value === '...') {
      this.next();
      const on = this.next();
      if (on.kind !== 'name' || on.value !== 'on') throw new Error('graphql: only inline fragments are supported');
      const typeCondition = this.expectName();
      return { type: 'fragment', typeCondition, selections: this.parseSelectionSet() };
    }
    // A field: [alias :] name [(args)] [selection set].
    let name = this.expectName();
    let alias: string | null = null;
    if (this.peek().kind === 'punct' && (this.peek() as { value: string }).value === ':') {
      this.next();
      alias = name;
      name = this.expectName();
    }
    let args = new Map<string, Value>();
    if (this.peek().kind === 'punct' && (this.peek() as { value: string }).value === '(') {
      args = this.parseArguments();
    }
    let selections: SelectionNode[] = [];
    if (this.peek().kind === 'punct' && (this.peek() as { value: string }).value === '{') {
      selections = this.parseSelectionSet();
    }
    return { type: 'field', alias, name, args, selections };
  }

  private parseArguments(): Map<string, Value> {
    this.expectPunct('(');
    const args = new Map<string, Value>();
    while (true) {
      const token = this.peek();
      if (token.kind === 'punct' && token.value === ')') {
        this.next();
        return args;
      }
      const name = this.expectName();
      this.expectPunct(':');
      args.set(name, this.parseValue());
    }
  }

  private parseValue(): Value {
    const token = this.next();
    if (token.kind === 'string' || token.kind === 'int' || token.kind === 'float') return token.value;
    if (token.kind === 'name') {
      if (token.value === 'true') return true;
      if (token.value === 'false') return false;
      if (token.value === 'null') return null;
      return { kind: 'enum', value: token.value };
    }
    if (token.kind === 'punct' && token.value === '$') {
      return { kind: 'variable', name: this.expectName() };
    }
    if (token.kind === 'punct' && token.value === '[') {
      const values: Value[] = [];
      while (!(this.peek().kind === 'punct' && (this.peek() as { value: string }).value === ']')) {
        values.push(this.parseValue());
      }
      this.next();
      return values;
    }
    if (token.kind === 'punct' && token.value === '{') {
      const object: { [key: string]: Value } = {};
      while (!(this.peek().kind === 'punct' && (this.peek() as { value: string }).value === '}')) {
        const name = this.expectName();
        this.expectPunct(':');
        object[name] = this.parseValue();
      }
      this.next();
      return object;
    }
    throw new Error('graphql: unexpected value');
  }
}

// --------------------------------------------------------------------------- values

/** A resolved object: its GraphQL type and the store object or data behind it. */
interface GqlObject {
  readonly __typename: string;
  readonly __value: unknown;
}

function obj(typename: string, value: unknown): GqlObject {
  return { __typename: typename, __value: value };
}

/** Substitute variables in an argument value; enums become their plain string. */
function resolveValue(value: Value, variables: Record<string, unknown>): unknown {
  if (typeof value === 'object' && value !== null && 'kind' in value) {
    if (value.kind === 'variable') return variables[value.name] ?? null;
    if (value.kind === 'enum') return value.value;
  }
  if (Array.isArray(value)) return value.map((item) => resolveValue(item, variables));
  if (typeof value === 'object' && value !== null) {
    const out: Record<string, unknown> = {};
    for (const [key, item] of Object.entries(value)) out[key] = resolveValue(item, variables);
    return out;
  }
  return value;
}

function argsOf(field: FieldNode, variables: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [key, value] of field.args) out[key] = resolveValue(value, variables);
  return out;
}

// --------------------------------------------------------------------------- global ids

/** An opaque, stable global id: base64url("<Type>:<key>"). Never the bare numeric id. */
export function globalId(type: string, key: string): string {
  return Buffer.from(`${type}:${key}`, 'utf8').toString('base64url');
}

function parseGlobalId(id: unknown): { type: string; key: string } | null {
  if (typeof id !== 'string') return null;
  try {
    const decoded = Buffer.from(id, 'base64url').toString('utf8');
    const at = decoded.indexOf(':');
    if (at === -1) return null;
    return { type: decoded.slice(0, at), key: decoded.slice(at + 1) };
  } catch {
    return null;
  }
}

// --------------------------------------------------------------------------- schema

/** The fields each type answers (validation: an unknown field is an error naming it). */
const FIELDS: Record<string, Set<string>> = {
  Query: new Set(['viewer', 'repository', 'node', '__type']),
  Mutation: new Set(['createIssue', 'closeIssue', 'createPullRequest', 'mergePullRequest', 'addComment']),
  User: new Set(['login', 'id']),
  Repository: new Set([
    'id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission',
    'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest',
  ]),
  Ref: new Set(['name']),
  Issue: new Set(['id', 'number', 'title', 'state', 'body']),
  IssueConnection: new Set(['nodes', 'pageInfo', 'totalCount']),
  PageInfo: new Set(['hasNextPage', 'endCursor', 'hasPreviousPage', 'startCursor']),
  PullRequest: new Set([
    'id', 'number', 'title', 'state', 'merged', 'headRefName', 'baseRefName', 'mergeable', 'files',
  ]),
  PullRequestConnection: new Set(['nodes', 'pageInfo', 'totalCount']),
  PullRequestChangedFile: new Set(['path']),
  IssueComment: new Set(['id', 'body']),
  CommentEdge: new Set(['node']),
  CreateIssuePayload: new Set(['issue']),
  CloseIssuePayload: new Set(['issue']),
  CreatePullRequestPayload: new Set(['pullRequest']),
  MergePullRequestPayload: new Set(['pullRequest']),
  AddCommentPayload: new Set(['commentEdge']),
  __Type: new Set(['name', 'fields', 'enumValues']),
  __Field: new Set(['name']),
  __EnumValue: new Set(['name']),
};

/** The introspection data: field and enum names per type (extra names are harmless). */
const TYPE_INFO: Record<string, { fields: string[]; enumValues: string[] }> = {
  Issue: {
    fields: ['id', 'number', 'title', 'state', 'body', 'createdAt', 'updatedAt', 'closed', 'author', 'comments', 'labels'],
    enumValues: [],
  },
  PullRequest: {
    fields: ['id', 'number', 'title', 'state', 'merged', 'headRefName', 'baseRefName', 'mergeable', 'files', 'body', 'createdAt', 'updatedAt', 'author'],
    enumValues: [],
  },
  Repository: {
    fields: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest', 'description', 'isPrivate', 'createdAt', 'updatedAt'],
    enumValues: [],
  },
  SearchType: { fields: [], enumValues: ['ISSUE', 'REPOSITORY', 'USER'] },
  User: { fields: ['login', 'id', 'name'], enumValues: [] },
  IssueState: { fields: [], enumValues: ['OPEN', 'CLOSED'] },
  MergeableState: { fields: [], enumValues: ['MERGEABLE', 'CONFLICTING', 'UNKNOWN'] },
  PullRequestState: { fields: [], enumValues: ['OPEN', 'CLOSED', 'MERGED'] },
};

/** The cursor of an issue in a connection: base64url("cursor:<createdSeq>"). */
function cursorOf(issue: Issue): string {
  return Buffer.from(`cursor:${issue.createdSeq}`, 'utf8').toString('base64url');
}

function seqOfCursor(cursor: unknown): number | null {
  if (typeof cursor !== 'string') return null;
  try {
    const decoded = Buffer.from(cursor, 'base64url').toString('utf8');
    if (!decoded.startsWith('cursor:')) return null;
    const seq = parseInt(decoded.slice('cursor:'.length), 10);
    return Number.isFinite(seq) ? seq : null;
  } catch {
    return null;
  }
}

// --------------------------------------------------------------------------- executor

class Executor {
  readonly errors: { message: string }[] = [];
  private readonly store: Store;
  constructor(store: Store) {
    this.store = store;
  }

  /** Execute one operation's selection set against the Query/Mutation root. */
  execute(operation: OperationNode, variables: Record<string, unknown>): Record<string, unknown> {
    const root = obj(operation.kind === 'mutation' ? 'Mutation' : 'Query', null);
    return this.executeSelections(root, operation.selections, variables) as Record<string, unknown>;
  }

  private executeSelections(parent: GqlObject, selections: SelectionNode[], variables: Record<string, unknown>): unknown {
    const out: Record<string, unknown> = {};
    for (const selection of selections) {
      if (selection.type === 'fragment') {
        if (selection.typeCondition !== parent.__typename) continue;
        const merged = this.executeSelections(parent, selection.selections, variables) as Record<string, unknown>;
        Object.assign(out, merged);
        continue;
      }
      const key = selection.alias ?? selection.name;
      out[key] = this.executeField(parent, selection, variables);
    }
    return out;
  }

  private executeField(parent: GqlObject, field: FieldNode, variables: Record<string, unknown>): unknown {
    const allowed = FIELDS[parent.__typename];
    if (field.name === '__typename') return parent.__typename;
    if (!allowed || !allowed.has(field.name)) {
      this.errors.push({ message: `Cannot query field "${field.name}" on type "${parent.__typename}".` });
      return null;
    }
    const args = argsOf(field, variables);
    const resolved = this.resolve(parent, field.name, args);
    if (resolved === null || resolved === undefined) return null;
    if (Array.isArray(resolved)) {
      return resolved.map((item) => this.complete(item, field.selections, variables));
    }
    if (typeof resolved === 'object' && resolved !== null && '__typename' in (resolved as object)) {
      return this.complete(resolved as GqlObject, field.selections, variables);
    }
    return resolved;
  }

  /** Complete a resolved value: scalars pass through, objects resolve their sub-selections. */
  private complete(value: unknown, selections: SelectionNode[], variables: Record<string, unknown>): unknown {
    if (value === null || value === undefined) return null;
    if (Array.isArray(value)) return value.map((item) => this.complete(item, selections, variables));
    if (typeof value === 'object' && value !== null && '__typename' in (value as object)) {
      if (selections.length === 0) return {};
      return this.executeSelections(value as GqlObject, selections, variables);
    }
    return value;
  }

  /** The field resolvers, by parent type. */
  private resolve(parent: GqlObject, field: string, args: Record<string, unknown>): unknown {
    const store = this.store;
    switch (parent.__typename) {
      case 'Query': {
        if (field === 'viewer') return obj('User', null);
        if (field === 'repository') {
          const repo = store.getRepo(String(args.owner ?? ''), String(args.name ?? ''));
          return repo === null ? null : obj('Repository', repo);
        }
        if (field === 'node') {
          const parsed = parseGlobalId(args.id);
          if (parsed === null || parsed.type !== 'Repository') return null;
          const repo = store.listRepos().find((candidate) => candidate.id === parseInt(parsed.key, 10));
          return repo === undefined ? null : obj('Repository', repo);
        }
        if (field === '__type') {
          const info = TYPE_INFO[String(args.name ?? '')];
          if (!info) return null;
          return obj('__Type', { name: String(args.name), ...info });
        }
        return null;
      }
      case 'Mutation': {
        if (field === 'createIssue') return this.mutateCreateIssue(args);
        if (field === 'closeIssue') return this.mutateCloseIssue(args);
        if (field === 'createPullRequest') return this.mutateCreatePullRequest(args);
        if (field === 'mergePullRequest') return this.mutateMergePullRequest(args);
        if (field === 'addComment') return this.mutateAddComment(args);
        return null;
      }
      case 'User': {
        if (field === 'login') return store.viewerLogin();
        if (field === 'id') return globalId('User', '1000');
        return null;
      }
      case 'Repository': {
        const repo = parent.__value as Repo;
        switch (field) {
          case 'id': return globalId('Repository', String(repo.id));
          case 'databaseId': return repo.id;
          case 'name': return repo.name;
          case 'nameWithOwner': return `${store.viewerLogin()}/${repo.name}`;
          case 'owner': return obj('User', null);
          case 'defaultBranchRef': return obj('Ref', { name: repo.defaultBranch });
          case 'viewerPermission': return 'ADMIN';
          case 'hasIssuesEnabled': return true;
          case 'issue': {
            const issue = store.getIssue(repo, Number(args.number));
            return issue === null ? null : obj('Issue', issue);
          }
          case 'issues': return this.issuesConnection(repo, args);
          case 'pullRequests': {
            const pulls = store.listPulls(repo, 'all');
            const first = typeof args.first === 'number' ? args.first : pulls.length;
            return obj('PullRequestConnection', { nodes: pulls.slice(0, first).map((pull) => obj('PullRequest', { repo, pull })) });
          }
          case 'pullRequest': {
            const pull = store.getPull(repo, Number(args.number));
            return pull === null ? null : obj('PullRequest', { repo, pull });
          }
          default: return null;
        }
      }
      case 'Ref': {
        return (parent.__value as { name: string }).name;
      }
      case 'Issue': {
        const issue = parent.__value as Issue;
        switch (field) {
          case 'id': return globalId('Issue', `${(parent.__value as Issue).id}`);
          case 'number': return issue.number;
          case 'title': return issue.title;
          case 'state': return issue.state === 'open' ? 'OPEN' : 'CLOSED';
          case 'body': return issue.body;
          default: return null;
        }
      }
      case 'IssueConnection': {
        const connection = parent.__value as { nodes: GqlObject[]; pageInfo: GqlObject };
        if (field === 'nodes') return connection.nodes;
        if (field === 'pageInfo') return connection.pageInfo;
        if (field === 'totalCount') return connection.nodes.length;
        return null;
      }
      case 'PageInfo': {
        const info = parent.__value as { hasNextPage: boolean; endCursor: string | null };
        if (field === 'hasNextPage') return info.hasNextPage;
        if (field === 'endCursor') return info.endCursor;
        if (field === 'hasPreviousPage') return false;
        if (field === 'startCursor') return null;
        return null;
      }
      case 'PullRequest': {
        const { repo, pull } = parent.__value as { repo: Repo; pull: PullRequest };
        switch (field) {
          case 'id': return globalId('PullRequest', `${pull.id}`);
          case 'number': return pull.number;
          case 'title': return pull.title;
          case 'state': return pull.merged ? 'MERGED' : pull.state === 'open' ? 'OPEN' : 'CLOSED';
          case 'merged': return pull.merged;
          case 'headRefName': return pull.headRef;
          case 'baseRefName': return pull.baseRef;
          case 'mergeable': return store.pullMergeable(repo, pull) ? 'MERGEABLE' : 'CONFLICTING';
          case 'files': {
            const first = typeof args.first === 'number' ? args.first : 10;
            const paths = store.pullFilePaths(repo, pull).slice(0, first);
            return obj('PullRequestConnection', { nodes: paths.map((path) => obj('PullRequestChangedFile', { path })) });
          }
          default: return null;
        }
      }
      case 'PullRequestConnection': {
        const connection = parent.__value as { nodes: GqlObject[] };
        if (field === 'nodes') return connection.nodes;
        if (field === 'pageInfo') return obj('PageInfo', { hasNextPage: false, endCursor: null });
        if (field === 'totalCount') return connection.nodes.length;
        return null;
      }
      case 'PullRequestChangedFile': {
        return (parent.__value as { path: string }).path;
      }
      case 'IssueComment': {
        const comment = parent.__value as { id: number; body: string; repoId: number };
        if (field === 'id') return globalId('IssueComment', `${comment.repoId}:${comment.id}`);
        if (field === 'body') return comment.body;
        return null;
      }
      case 'CommentEdge': {
        if (field === 'node') return (parent.__value as { node: GqlObject }).node;
        return null;
      }
      case 'CreateIssuePayload':
      case 'CloseIssuePayload': {
        if (field === 'issue') return (parent.__value as { issue: GqlObject | null }).issue;
        return null;
      }
      case 'CreatePullRequestPayload':
      case 'MergePullRequestPayload': {
        if (field === 'pullRequest') return (parent.__value as { pullRequest: GqlObject | null }).pullRequest;
        return null;
      }
      case 'AddCommentPayload': {
        if (field === 'commentEdge') return (parent.__value as { commentEdge: GqlObject }).commentEdge;
        return null;
      }
      case '__Type': {
        const info = parent.__value as { name: string; fields: string[]; enumValues: string[] };
        if (field === 'name') return info.name;
        if (field === 'fields') return info.fields.map((name) => obj('__Field', { name }));
        if (field === 'enumValues') return info.enumValues.map((name) => obj('__EnumValue', { name }));
        return null;
      }
      case '__Field':
      case '__EnumValue': {
        return (parent.__value as { name: string }).name;
      }
      default:
        return null;
    }
  }

  /** The issues connection: states filter, orderBy, first/after pagination. */
  private issuesConnection(repo: Repo, args: Record<string, unknown>): GqlObject {
    let issues = [...repo.issues];
    const states = args.states;
    if (Array.isArray(states) && states.length > 0) {
      const wanted = new Set(states.map((state) => String(state).toLowerCase()));
      issues = issues.filter((issue) => wanted.has(issue.state));
    }
    const orderBy = args.orderBy as { field?: string; direction?: string } | undefined;
    const direction = orderBy?.direction === 'ASC' ? 1 : -1;
    issues.sort((a, b) => (a.createdSeq - b.createdSeq) * direction);
    const afterSeq = seqOfCursor(args.after);
    if (afterSeq !== null) {
      issues = direction === 1
        ? issues.filter((issue) => issue.createdSeq > afterSeq)
        : issues.filter((issue) => issue.createdSeq < afterSeq);
    }
    const first = typeof args.first === 'number' ? args.first : issues.length;
    const page = issues.slice(0, first);
    const hasNextPage = issues.length > page.length;
    const endCursor = page.length > 0 ? cursorOf(page[page.length - 1]) : null;
    return obj('IssueConnection', {
      nodes: page.map((issue) => obj('Issue', issue)),
      pageInfo: obj('PageInfo', { hasNextPage, endCursor }),
    });
  }

  // --------------------------------------------------------------------- mutations

  private repoByGlobalId(id: unknown): Repo | null {
    const parsed = parseGlobalId(id);
    if (parsed === null || parsed.type !== 'Repository') return null;
    return this.store.listRepos().find((repo) => repo.id === parseInt(parsed.key, 10)) ?? null;
  }

  private issueByGlobalId(id: unknown): { repo: Repo; issue: Issue } | null {
    const parsed = parseGlobalId(id);
    if (parsed === null || parsed.type !== 'Issue') return null;
    // Issue global ids carry the issue's store id; find its repo by scanning.
    const issueId = parseInt(parsed.key, 10);
    if (!Number.isFinite(issueId)) return null;
    for (const repo of this.store.listRepos()) {
      const issue = repo.issues.find((candidate) => candidate.id === issueId);
      if (issue) return { repo, issue };
    }
    return null;
  }

  private pullByGlobalId(id: unknown): { repo: Repo; pull: PullRequest } | null {
    const parsed = parseGlobalId(id);
    if (parsed === null || parsed.type !== 'PullRequest') return null;
    const pullId = parseInt(parsed.key, 10);
    if (!Number.isFinite(pullId)) return null;
    for (const repo of this.store.listRepos()) {
      const pull = repo.pulls.find((candidate) => candidate.id === pullId);
      if (pull) return { repo, pull };
    }
    return null;
  }

  private mutateCreateIssue(args: Record<string, unknown>): GqlObject {
    const input = (args.input ?? {}) as Record<string, unknown>;
    const repo = this.repoByGlobalId(input.repositoryId);
    if (!repo) {
      this.errors.push({ message: 'repository not found' });
      return obj('CreateIssuePayload', { issue: null });
    }
    const issue = this.store.createIssue(repo, { title: String(input.title ?? ''), body: String(input.body ?? '') });
    if (!issue) {
      this.errors.push({ message: 'could not create the issue' });
      return obj('CreateIssuePayload', { issue: null });
    }
    return obj('CreateIssuePayload', { issue: obj('Issue', issue) });
  }

  private mutateCloseIssue(args: Record<string, unknown>): GqlObject {
    const input = (args.input ?? {}) as Record<string, unknown>;
    const found = this.issueByGlobalId(input.issueId);
    if (!found) {
      this.errors.push({ message: 'issue not found' });
      return obj('CloseIssuePayload', { issue: null });
    }
    const issue = this.store.patchIssue(found.repo, found.issue.number, { state: 'closed' });
    return obj('CloseIssuePayload', { issue: issue === null ? null : obj('Issue', issue) });
  }

  private mutateCreatePullRequest(args: Record<string, unknown>): GqlObject {
    const input = (args.input ?? {}) as Record<string, unknown>;
    const repo = this.repoByGlobalId(input.repositoryId);
    if (!repo) {
      this.errors.push({ message: 'repository not found' });
      return obj('CreatePullRequestPayload', { pullRequest: null });
    }
    const pull = this.store.createPullRequest(repo, {
      title: String(input.title ?? ''),
      body: String(input.body ?? ''),
      head: String(input.headRefName ?? ''),
      base: String(input.baseRefName ?? repo.defaultBranch),
    });
    if (!pull) {
      this.errors.push({ message: 'head or base branch does not exist' });
      return obj('CreatePullRequestPayload', { pullRequest: null });
    }
    return obj('CreatePullRequestPayload', { pullRequest: obj('PullRequest', { repo, pull }) });
  }

  private mutateMergePullRequest(args: Record<string, unknown>): GqlObject {
    const input = (args.input ?? {}) as Record<string, unknown>;
    const found = this.pullByGlobalId(input.pullRequestId);
    if (!found) {
      this.errors.push({ message: 'pull request not found' });
      return obj('MergePullRequestPayload', { pullRequest: null });
    }
    const merged = this.store.mergePull(found.repo, found.pull.number);
    if ('error' in merged) {
      this.errors.push({ message: `could not merge the pull request: ${merged.error}` });
      return obj('MergePullRequestPayload', { pullRequest: obj('PullRequest', { repo: found.repo, pull: found.pull }) });
    }
    return obj('MergePullRequestPayload', { pullRequest: obj('PullRequest', { repo: found.repo, pull: found.pull }) });
  }

  private mutateAddComment(args: Record<string, unknown>): GqlObject {
    const input = (args.input ?? {}) as Record<string, unknown>;
    const found = this.issueByGlobalId(input.subjectId);
    if (!found) {
      this.errors.push({ message: 'subject not found' });
      return obj('AddCommentPayload', { commentEdge: obj('CommentEdge', { node: null }) });
    }
    const comment = this.store.addComment(found.repo, found.issue.number, String(input.body ?? ''));
    if (!comment) {
      this.errors.push({ message: 'could not add the comment' });
      return obj('AddCommentPayload', { commentEdge: obj('CommentEdge', { node: null }) });
    }
    const node = obj('IssueComment', { id: comment.id, body: comment.body, repoId: found.repo.id });
    return obj('AddCommentPayload', { commentEdge: obj('CommentEdge', { node }) });
  }
}

// --------------------------------------------------------------------------- handler

/** Execute one {query, variables, operationName?} envelope; always HTTP 200 on a parsed body. */
function executeOne(store: Store, envelope: unknown): Record<string, unknown> {
  const record = (typeof envelope === 'object' && envelope !== null ? envelope : {}) as Record<string, unknown>;
  const query = record.query;
  if (typeof query !== 'string') {
    return { data: null, errors: [{ message: 'query must be a string' }] };
  }
  const variables = (typeof record.variables === 'object' && record.variables !== null
    ? record.variables
    : {}) as Record<string, unknown>;
  let operations: OperationNode[];
  try {
    operations = new Parser(lex(query)).parseDocument();
  } catch (err) {
    return { data: null, errors: [{ message: err instanceof Error ? err.message : 'could not parse the query' }] };
  }
  if (operations.length === 0) {
    return { data: null, errors: [{ message: 'no operation in the document' }] };
  }
  const operationName = typeof record.operationName === 'string' ? record.operationName : null;
  const operation = operationName !== null
    ? operations.find((candidate) => candidate.name === operationName)
    : operations[0];
  if (!operation) {
    return { data: null, errors: [{ message: `unknown operation ${operationName}` }] };
  }
  const executor = new Executor(store);
  const data = executor.execute(operation, variables);
  if (executor.errors.length > 0) return { data, errors: executor.errors };
  return { data };
}

/**
 * Handle POST /api/graphql: a single envelope or a batch (a JSON array, answered as an array in
 * request order). A body that is not JSON is a 400 (the router handles malformed JSON for the
 * REST surface; here the same rule applies).
 */
export function handleGraphQL(store: Store, body: Buffer): RestReply {
  let parsed: unknown;
  try {
    parsed = JSON.parse(body.toString('utf8'));
  } catch {
    return { status: 400, json: { message: 'Problems parsing JSON' } };
  }
  if (Array.isArray(parsed)) {
    return { status: 200, json: parsed.map((envelope) => executeOne(store, envelope)) };
  }
  return { status: 200, json: executeOne(store, parsed) };
}
