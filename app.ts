// The HTTP server: one process, one store, four surfaces. main.ts owns the lifecycle (bind an
// ephemeral port, register it with the env, poll /done); this file owns the routing.
//
//   GET  /healthz                        200 {"ok":true} — the env's boot probe, no token.
//   /api/v3/...                          REST v3 (auth, except the root and /meta).
//   POST /api/graphql                    GraphQL v4 (auth).
//   POST /mcp                            MCP over Streamable HTTP (auth).
//   /{owner}/{repo}.git/...              git smart HTTP (auth).
//   anything else                        404 {"message":"Not Found"}.
//
// Auth is Bearer $MAKEFUN_SHOP_TOKEN; a request without it (or with the wrong token) is 401 with
// a JSON body. Every REST reply carries a non-empty X-GitHub-Request-Id. No handler may throw
// out of the server: an unexpected error is a 500 JSON reply and the process stays up.

import { createServer, type IncomingMessage, type Server, type ServerResponse } from 'node:http';
import { handleGraphQL } from './graphql.ts';
import { handleInfoRefs, handleReceivePack, handleUploadPack } from './git-http.ts';
import { handleMcp } from './mcp.ts';
import { handleRest, type RestRequest } from './rest.ts';
import { Store } from './store.ts';

/** The bearer token every API call must carry ('' when the env did not set one). */
const TOKEN = process.env.MAKEFUN_SHOP_TOKEN ?? '';

/** The largest request body accepted (the biggest push is a few hundred KiB). */
const MAX_BODY_BYTES = 64 * 1024 * 1024;

let requestCounter = 0;

/** The next request id: deterministic, non-empty, unique within the trial. */
function nextRequestId(): string {
  requestCounter += 1;
  return `req-${requestCounter.toString(16).padStart(8, '0')}`;
}

/** True when the request carries the trial's bearer token. */
function authorized(req: IncomingMessage): boolean {
  if (TOKEN === '') return false;
  return req.headers.authorization === `Bearer ${TOKEN}`;
}

function sendJson(res: ServerResponse, status: number, value: unknown, extraHeaders: Record<string, string> = {}): void {
  const body = `${JSON.stringify(value)}\n`;
  res.writeHead(status, {
    'content-type': 'application/json; charset=utf-8',
    'content-length': Buffer.byteLength(body),
    'x-github-request-id': nextRequestId(),
    ...extraHeaders,
  });
  res.end(body);
}

function sendBuffer(res: ServerResponse, status: number, headers: Record<string, string>, body: Buffer): void {
  res.writeHead(status, { 'content-length': body.length, 'x-github-request-id': nextRequestId(), ...headers });
  res.end(body);
}

/** Read the whole request body (up to the cap), or null when it overruns it. */
function readBody(req: IncomingMessage): Promise<Buffer | null> {
  return new Promise((resolve) => {
    const chunks: Buffer[] = [];
    let length = 0;
    let over = false;
    req.on('data', (chunk: Buffer) => {
      if (over) return;
      length += chunk.length;
      if (length > MAX_BODY_BYTES) {
        over = true;
        resolve(null);
        return;
      }
      chunks.push(chunk);
    });
    req.on('end', () => {
      if (!over) resolve(Buffer.concat(chunks));
    });
    req.on('error', () => resolve(Buffer.alloc(0)));
  });
}

/** Split a path into decoded segments, dropping empty ones (so "/api/v3/" and "/api/v3" agree). */
function segmentsOf(pathname: string): string[] {
  return pathname
    .split('/')
    .filter((segment) => segment.length > 0)
    .map((segment) => {
      try {
        return decodeURIComponent(segment);
      } catch {
        return segment;
      }
    });
}

/** Create the server. The store is created once, here, and shared by every surface. */
export function createApp(): Server {
  const store = new Store();

  return createServer(async (req, res) => {
    try {
      const url = new URL(req.url ?? '/', `http://${req.headers.host ?? 'localhost'}`);
      const method = req.method ?? 'GET';
      const segments = segmentsOf(url.pathname);

      // The boot probe: no token, always 200.
      if (method === 'GET' && segments.length === 1 && segments[0] === 'healthz') {
        sendJson(res, 200, { ok: true });
        return;
      }

      // GraphQL v4.
      if (segments.length === 2 && segments[0] === 'api' && segments[1] === 'graphql') {
        if (!authorized(req)) {
          sendJson(res, 401, { message: 'Requires authentication' });
          return;
        }
        if (method !== 'POST') {
          sendJson(res, 405, { message: 'Method Not Allowed' });
          return;
        }
        const body = await readBody(req);
        if (body === null) {
          sendJson(res, 413, { message: 'Request body too large' });
          return;
        }
        const reply = handleGraphQL(store, body);
        if (reply.json !== undefined) sendJson(res, reply.status, reply.json, reply.headers);
        else sendJson(res, reply.status, {});
        return;
      }

      // MCP.
      if (segments.length === 1 && segments[0] === 'mcp') {
        if (!authorized(req)) {
          sendJson(res, 401, { message: 'Requires authentication' });
          return;
        }
        if (method === 'DELETE') {
          // A legacy session close: acknowledged, unscored.
          sendJson(res, 200, {});
          return;
        }
        if (method !== 'POST') {
          sendJson(res, 405, { message: 'Method Not Allowed' });
          return;
        }
        const body = await readBody(req);
        if (body === null) {
          sendJson(res, 413, { message: 'Request body too large' });
          return;
        }
        const reply = handleMcp(store, body);
        if (reply.json !== undefined) sendJson(res, reply.status, reply.json, reply.headers);
        else sendJson(res, reply.status, {});
        return;
      }

      // REST v3.
      if (segments.length >= 1 && segments[0] === 'api' && segments[1] === 'v3') {
        const restSegments = segments.slice(2);
        const needsAuth = !(restSegments.length === 0 || (restSegments.length === 1 && restSegments[0] === 'meta'));
        if (needsAuth && !authorized(req)) {
          sendJson(res, 401, { message: 'Requires authentication' });
          return;
        }
        const body = await readBody(req);
        if (body === null) {
          sendJson(res, 413, { message: 'Request body too large' });
          return;
        }
        const restReq: RestRequest = {
          method,
          segments: restSegments,
          query: url.searchParams,
          body,
          host: req.headers.host ?? 'localhost',
        };
        const reply = handleRest(store, restReq);
        if (reply.status === 204) {
          res.writeHead(204, { 'x-github-request-id': nextRequestId() });
          res.end();
          return;
        }
        if (reply.json !== undefined) sendJson(res, reply.status, reply.json, reply.headers);
        else if (reply.raw !== undefined) sendBuffer(res, reply.status, { 'content-type': 'application/json; charset=utf-8' }, Buffer.from(reply.raw));
        else sendJson(res, reply.status, {});
        return;
      }

      // Git smart HTTP: /{owner}/{repo}.git/info/refs | git-upload-pack | git-receive-pack.
      if (segments.length >= 3 && segments[1].endsWith('.git')) {
        if (!authorized(req)) {
          sendJson(res, 401, { message: 'Requires authentication' });
          return;
        }
        const owner = segments[0];
        const repoName = segments[1].slice(0, -'.git'.length);
        const service = segments[2];
        if (service === 'info' && segments[3] === 'refs' && method === 'GET') {
          const reply = handleInfoRefs(store, owner, repoName, url.searchParams.get('service') ?? '', req.headers['git-protocol']);
          sendBuffer(res, reply.status, reply.headers, reply.body);
          return;
        }
        if (service === 'git-upload-pack' && method === 'POST') {
          const body = await readBody(req);
          if (body === null) {
            sendJson(res, 413, { message: 'Request body too large' });
            return;
          }
          const reply = handleUploadPack(store, owner, repoName, body, req.headers['git-protocol']);
          sendBuffer(res, reply.status, reply.headers, reply.body);
          return;
        }
        if (service === 'git-receive-pack' && method === 'POST') {
          const body = await readBody(req);
          if (body === null) {
            sendJson(res, 413, { message: 'Request body too large' });
            return;
          }
          const reply = handleReceivePack(store, owner, repoName, body);
          sendBuffer(res, reply.status, reply.headers, reply.body);
          return;
        }
        sendJson(res, 404, { message: 'Not Found' });
        return;
      }

      // Anything else: a JSON 404, never HTML.
      sendJson(res, 404, { message: 'Not Found' });
    } catch (err) {
      process.stderr.write(`[entry] ${err instanceof Error ? err.stack ?? err.message : String(err)}\n`);
      if (!res.headersSent) sendJson(res, 500, { message: 'Internal Server Error' });
      else res.end();
    }
  });
}
