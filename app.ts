// The HTTP server: one process, one store, four surfaces. main.ts owns the lifecycle (bind an
// ephemeral port, register it with the env, poll /done); this file owns the routing. The wire
// is a raw-TCP HTTP/1.1 server (http.ts) instead of node:http: the handler sees each request
// only once its whole body has arrived, so dispatch is fully synchronous — no stream waits.
//
//   GET  /healthz                        200 {"ok":true} — the env's boot probe, no token.
//   /api/v3/...                          REST v3 (auth, except the root and /meta).
//   POST /api/graphql                    GraphQL v4 (auth).
//   POST /mcp                            MCP over Streamable HTTP (auth).
//   /{owner}/{repo}.git/...              git smart HTTP (auth).
//   anything else                        404 {"message":"Not Found"}.
//
// Auth is Bearer $MAKEFUN_SHOP_TOKEN; a request without it (or with the wrong token) is 401 with
// a JSON body. Every reply carries a non-empty X-GitHub-Request-Id (added by http.ts). No handler
// may throw out of dispatch: an unexpected error is a 500 JSON reply and the process stays up.

import type { Server } from 'node:net';
import { createHttpServer, type HttpRequest, type HttpResponse } from './http.ts';
import { handleGraphQL } from './graphql.ts';
import { handleInfoRefs, handleReceivePack, handleUploadPack } from './git-http.ts';
import { handleMcp } from './mcp.ts';
import { handleRest, type RestRequest } from './rest.ts';
import { Store } from './store.ts';

/** The bearer token every API call must carry ('' when the env did not set one). */
const TOKEN = process.env.MAKEFUN_SHOP_TOKEN ?? '';

/** The full Authorization header value, built once: per-request template strings are waste. */
const BEARER = `Bearer ${TOKEN}`;

/** Shared empty query params for requests without a query string (read-only use). */
const EMPTY_PARAMS = new URLSearchParams();

/** The healthz body, pre-built: the boot probe answers the same bytes every time. */
const HEALTHZ_BODY = Buffer.from('{"ok":true}\n', 'utf8');

/** The constant error bodies, pre-built: no JSON.stringify per error reply. */
const AUTH_REQUIRED_BODY = Buffer.from('{"message":"Requires authentication"}\n', 'utf8');
const NOT_FOUND_BODY = Buffer.from('{"message":"Not Found"}\n', 'utf8');
const METHOD_NOT_ALLOWED_BODY = Buffer.from('{"message":"Method Not Allowed"}\n', 'utf8');

/** The JSON content type every JSON reply carries. */
const JSON_TYPE = 'application/json; charset=utf-8';

/** True when the request carries the trial's bearer token. */
function authorized(req: HttpRequest): boolean {
  if (TOKEN === '') return false;
  return req.headers['authorization'] === BEARER;
}

function jsonReply(status: number, value: unknown, extraHeaders: Record<string, string> = {}): HttpResponse {
  return {
    status,
    headers: { 'content-type': JSON_TYPE, ...extraHeaders },
    body: Buffer.from(`${JSON.stringify(value)}\n`),
  };
}

function bufferReply(status: number, headers: Record<string, string>, body: Buffer): HttpResponse {
  return { status, headers, body };
}

/** Decode one path segment (a malformed escape keeps the raw segment). */
function decodeSegment(segment: string): string {
  try {
    return decodeURIComponent(segment);
  } catch {
    return segment;
  }
}

/**
 * Split a path into decoded segments, dropping empty ones (so "/api/v3/" and "/api/v3" agree).
 * One pass, no intermediate arrays: segments without '%' skip decodeURIComponent entirely
 * (it is a no-op for them), which is every segment the suite ever sends.
 */
function segmentsOf(pathname: string): string[] {
  const segments: string[] = [];
  let start = 0;
  for (let i = 0; i <= pathname.length; i += 1) {
    if (i === pathname.length || pathname.charCodeAt(i) === 47 /* '/' */) {
      if (i > start) {
        const segment = pathname.slice(start, i);
        segments.push(segment.indexOf('%') === -1 ? segment : decodeSegment(segment));
      }
      start = i + 1;
    }
  }
  return segments;
}

/**
 * The value of one query parameter, without building a URLSearchParams: the git client's
 * info/refs query is exactly `service=<name>`. Anything else falls back to the real parser.
 */
function queryParam(queryString: string, name: string): string | null {
  if (queryString === '') return null;
  const prefix = `${name}=`;
  if (queryString.startsWith(prefix) && queryString.indexOf('&') === -1) {
    const value = queryString.slice(prefix.length);
    return value.indexOf('%') === -1 ? value : decodeSegment(value);
  }
  return new URLSearchParams(queryString).get(name);
}

/** Route one fully-read request to its surface and build its reply. */
function dispatch(store: Store, req: HttpRequest): HttpResponse {
  // Parse the request target by hand: `new URL` plus URLSearchParams costs more than the
  // whole hot-path handler. A '#' truncates the target (a fragment never reaches the
  // server); backslashes are not normalised to slashes the way URL would (the suite never
  // sends them, and no route name contains one).
  let rawUrl = req.target;
  const hashAt = rawUrl.indexOf('#');
  if (hashAt !== -1) rawUrl = rawUrl.slice(0, hashAt);
  const queryAt = rawUrl.indexOf('?');
  const pathname = queryAt === -1 ? rawUrl : rawUrl.slice(0, queryAt);
  const queryString = queryAt === -1 ? '' : rawUrl.slice(queryAt + 1);
  const method = req.method;
  const segments = segmentsOf(pathname);

  // The boot probe: no token, always 200.
  if (method === 'GET' && segments.length === 1 && segments[0] === 'healthz') {
    return bufferReply(200, { 'content-type': JSON_TYPE }, HEALTHZ_BODY);
  }

  // GraphQL v4.
  if (segments.length === 2 && segments[0] === 'api' && segments[1] === 'graphql') {
    if (!authorized(req)) return bufferReply(401, { 'content-type': JSON_TYPE }, AUTH_REQUIRED_BODY);
    if (method !== 'POST') return bufferReply(405, { 'content-type': JSON_TYPE }, METHOD_NOT_ALLOWED_BODY);
    const reply = handleGraphQL(store, req.body);
    if (reply.json !== undefined) return jsonReply(reply.status, reply.json, reply.headers);
    return jsonReply(reply.status, {});
  }

  // MCP.
  if (segments.length === 1 && segments[0] === 'mcp') {
    if (!authorized(req)) return bufferReply(401, { 'content-type': JSON_TYPE }, AUTH_REQUIRED_BODY);
    if (method === 'DELETE') {
      // A legacy session close: acknowledged, unscored.
      return jsonReply(200, {});
    }
    if (method !== 'POST') return bufferReply(405, { 'content-type': JSON_TYPE }, METHOD_NOT_ALLOWED_BODY);
    const reply = handleMcp(store, req.body);
    if (reply.json !== undefined) return jsonReply(reply.status, reply.json, reply.headers);
    return jsonReply(reply.status, {});
  }

  // REST v3.
  if (segments.length >= 1 && segments[0] === 'api' && segments[1] === 'v3') {
    const restSegments = segments.slice(2);
    const needsAuth = !(restSegments.length === 0 || (restSegments.length === 1 && restSegments[0] === 'meta'));
    if (needsAuth && !authorized(req)) return bufferReply(401, { 'content-type': JSON_TYPE }, AUTH_REQUIRED_BODY);
    const restReq: RestRequest = {
      method,
      segments: restSegments,
      query: queryString === '' ? EMPTY_PARAMS : new URLSearchParams(queryString),
      body: req.body,
      host: req.headers['host'] ?? 'localhost',
    };
    const reply = handleRest(store, restReq);
    if (reply.status === 204) return { status: 204 };
    if (reply.json !== undefined) return jsonReply(reply.status, reply.json, reply.headers);
    if (reply.raw !== undefined) {
      // A cached reply may already be a Buffer: send it without re-encoding it.
      const raw = typeof reply.raw === 'string' ? Buffer.from(reply.raw) : reply.raw;
      return bufferReply(reply.status, { 'content-type': JSON_TYPE }, raw);
    }
    return jsonReply(reply.status, {});
  }

  // Git smart HTTP: /{owner}/{repo}.git/info/refs | git-upload-pack | git-receive-pack.
  if (segments.length >= 3 && segments[1].endsWith('.git')) {
    if (!authorized(req)) return bufferReply(401, { 'content-type': JSON_TYPE }, AUTH_REQUIRED_BODY);
    const owner = segments[0];
    const repoName = segments[1].slice(0, -'.git'.length);
    const service = segments[2];
    if (service === 'info' && segments[3] === 'refs' && method === 'GET') {
      const reply = handleInfoRefs(store, owner, repoName, queryParam(queryString, 'service') ?? '', req.headers['git-protocol']);
      return bufferReply(reply.status, reply.headers, reply.body);
    }
    if (service === 'git-upload-pack' && method === 'POST') {
      const reply = handleUploadPack(store, owner, repoName, req.body, req.headers['git-protocol']);
      return bufferReply(reply.status, reply.headers, reply.body);
    }
    if (service === 'git-receive-pack' && method === 'POST') {
      const reply = handleReceivePack(store, owner, repoName, req.body);
      return bufferReply(reply.status, reply.headers, reply.body);
    }
    return bufferReply(404, { 'content-type': JSON_TYPE }, NOT_FOUND_BODY);
  }

  // Anything else: a JSON 404, never HTML.
  return bufferReply(404, { 'content-type': JSON_TYPE }, NOT_FOUND_BODY);
}

/** Create the server. The store is created once, here, and shared by every surface. */
export function createApp(): Server {
  const store = new Store();
  return createHttpServer((req) => dispatch(store, req));
}
