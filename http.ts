// A minimal, fast HTTP/1.1 server on raw TCP (node:net), written for this entry's one client:
// the suite's undici fetch plus the git CLI over loopback. It implements exactly what those send:
//
//   - request line + headers (case-insensitive names), HTTP/1.0 and HTTP/1.1
//   - Content-Length bodies and chunked transfer-encoding (git POSTs use both)
//   - Expect: 100-continue (answered at once, before the body arrives)
//   - keep-alive (HTTP/1.1 default; HTTP/1.0 only on `Connection: keep-alive`), pipelined
//     requests answered in order
//   - HEAD (headers only), 204 (no content-length), connection close/error handling
//
// Everything is synchronous per socket: a request is dispatched only once its whole body has
// arrived, and the next request on the same socket is parsed only after the reply is written,
// so responses can never interleave. A dispatch error is a 500 JSON reply and the socket stays
// open; a malformed request is a 400 and the socket closes.

import { createServer, type Server, type Socket } from 'node:net';

/** One parsed request: the handler sees the whole body, already read. */
export interface HttpRequest {
  readonly method: string;
  readonly target: string;
  readonly version: string;
  readonly headers: Record<string, string>;
  readonly body: Buffer;
}

/** What the handler returns: status, extra headers, body (Buffer, string, or none). */
export interface HttpResponse {
  readonly status: number;
  readonly headers?: Record<string, string>;
  readonly body?: Buffer | string | null;
  /** Close the socket after this reply (default: follow keep-alive rules). */
  readonly close?: boolean;
}

export type HttpHandler = (req: HttpRequest) => HttpResponse;

/** The largest request head (request line + headers) accepted. */
const MAX_HEAD_BYTES = 64 * 1024;
/** The largest request body accepted (the biggest push is a few hundred KiB). */
const MAX_BODY_BYTES = 64 * 1024 * 1024;
/** Shared empty body (never mutated). */
const EMPTY_BODY = Buffer.alloc(0);
/** The 400 body, pre-built. */
const BAD_REQUEST_BODY = Buffer.from('{"message":"Bad Request"}\n', 'utf8');
/** The 413 body, pre-built. */
const TOO_LARGE_BODY = Buffer.from('{"message":"Request body too large"}\n', 'utf8');
/** The 500 body, pre-built. */
const SERVER_ERROR_BODY = Buffer.from('{"message":"Internal Server Error"}\n', 'utf8');
/** The JSON content type every error reply carries. */
const JSON_TYPE = 'application/json; charset=utf-8';

const REASONS: Record<number, string> = {
  200: 'OK',
  201: 'Created',
  204: 'No Content',
  400: 'Bad Request',
  401: 'Unauthorized',
  404: 'Not Found',
  405: 'Method Not Allowed',
  409: 'Conflict',
  413: 'Content Too Large',
  422: 'Unprocessable Entity',
  500: 'Internal Server Error',
};

let requestCounter = 0;

/** The next request id: deterministic, non-empty, unique within the trial. */
function nextRequestId(): string {
  requestCounter += 1;
  return `req-${requestCounter.toString(16).padStart(8, '0')}`;
}

/** Parse a Content-Length value by hand: all digits, or -1 when malformed. */
function parseContentLength(value: string): number {
  if (value.length === 0) return -1;
  let n = 0;
  for (let i = 0; i < value.length; i += 1) {
    const c = value.charCodeAt(i);
    if (c < 48 || c > 57) return -1;
    n = n * 10 + (c - 48);
    if (n > MAX_BODY_BYTES) return -2;
  }
  return n;
}

/** Parse a chunk-size line (hex digits, optional `;extension`): -1 when malformed. */
function parseChunkSize(line: string): number {
  const semi = line.indexOf(';');
  const digits = (semi === -1 ? line : line.slice(0, semi)).trim();
  if (digits.length === 0) return -1;
  let n = 0;
  for (let i = 0; i < digits.length; i += 1) {
    const c = digits.charCodeAt(i);
    let d: number;
    if (c >= 48 && c <= 57) d = c - 48;
    else if (c >= 97 && c <= 102) d = c - 87;
    else if (c >= 65 && c <= 70) d = c - 55;
    else return -1;
    n = n * 16 + d;
    if (n > MAX_BODY_BYTES) return -2;
  }
  return n;
}

/** One client connection: a small state machine over the socket's byte stream. */
class Connection {
  private pending: Buffer = EMPTY_BODY;
  private closed = false;
  private phase: 'head' | 'body' | 'chunk-size' | 'chunk-data' | 'trailer' = 'head';
  private method = '';
  private target = '';
  private version = '1.1';
  private headers: Record<string, string> = {};
  private keepAlive = true;
  private bodyLength = 0;
  private chunkRemaining = 0;
  private bodyParts: Buffer[] = [];
  private bodyBytes = 0;
  private readonly socket: Socket;
  private readonly handler: HttpHandler;

  constructor(socket: Socket, handler: HttpHandler) {
    this.socket = socket;
    this.handler = handler;
    socket.setNoDelay(true);
    socket.on('data', (data: Buffer) => this.feed(data));
    socket.on('error', () => this.destroy());
    socket.on('end', () => this.destroy());
  }

  private destroy(): void {
    if (this.closed) return;
    this.closed = true;
    this.socket.destroy();
  }

  private feed(data: Buffer): void {
    if (this.closed) return;
    this.pending = this.pending.length === 0 ? data : Buffer.concat([this.pending, data]);
    this.drain();
  }

  /** Parse and dispatch as many complete requests as the buffered bytes hold. */
  private drain(): void {
    for (;;) {
      if (this.closed) return;
      if (this.phase === 'head') {
        if (!this.parseHead()) return;
      } else if (this.phase === 'body') {
        if (this.pending.length < this.bodyLength) return;
        const body = this.pending.subarray(0, this.bodyLength);
        this.pending = this.pending.subarray(this.bodyLength);
        this.phase = 'head';
        this.dispatch(body);
      } else if (this.phase === 'chunk-size') {
        if (!this.parseChunkSizeLine()) return;
      } else if (this.phase === 'chunk-data') {
        if (!this.parseChunkData()) return;
      } else {
        if (!this.parseTrailers()) return;
      }
    }
  }

  /**
   * Parse the request line + headers. Returns false when more bytes are needed (or the socket
   * closed); on success it sets the framing and either dispatches at once (no body) or moves to
   * the body phase. A malformed head is a 400 and the socket closes.
   */
  private parseHead(): boolean {
    const end = this.pending.indexOf('\r\n\r\n');
    if (end === -1) {
      if (this.pending.length > MAX_HEAD_BYTES) this.fail(400);
      return false;
    }
    const headText = this.pending.subarray(0, end).toString('latin1');
    this.pending = this.pending.subarray(end + 4);
    const lines = headText.split('\r\n');
    const requestLine = lines[0] ?? '';
    const firstSpace = requestLine.indexOf(' ');
    const lastSpace = requestLine.lastIndexOf(' ');
    if (firstSpace <= 0 || lastSpace <= firstSpace) {
      this.fail(400);
      return false;
    }
    const versionToken = requestLine.slice(lastSpace + 1);
    if (versionToken !== 'HTTP/1.1' && versionToken !== 'HTTP/1.0') {
      this.fail(400);
      return false;
    }
    this.method = requestLine.slice(0, firstSpace);
    this.target = requestLine.slice(firstSpace + 1, lastSpace);
    this.version = versionToken.slice(5);
    const headers: Record<string, string> = {};
    for (let i = 1; i < lines.length; i += 1) {
      const line = lines[i] ?? '';
      const colon = line.indexOf(':');
      if (colon <= 0) {
        this.fail(400);
        return false;
      }
      const name = line.slice(0, colon).toLowerCase();
      if (headers[name] === undefined) headers[name] = line.slice(colon + 1).trim();
    }
    this.headers = headers;

    const connection = headers['connection'] ?? '';
    if (this.version === '1.1') this.keepAlive = connection.toLowerCase() !== 'close';
    else this.keepAlive = connection.toLowerCase() === 'keep-alive';

    // A client that asked for 100-continue waits for it before sending the body: answer at
    // once, or it stalls a full second (libcurl's default) before sending anyway.
    const expect = headers['expect'] ?? '';
    if (expect.toLowerCase().indexOf('100-continue') !== -1) {
      this.socket.write(`HTTP/${this.version} 100 Continue\r\n\r\n`);
    }

    const transferEncoding = headers['transfer-encoding'] ?? '';
    if (transferEncoding.toLowerCase().indexOf('chunked') !== -1) {
      // Chunked wins over Content-Length (RFC 7230 3.3.3), as in node:http.
      this.bodyParts = [];
      this.bodyBytes = 0;
      this.phase = 'chunk-size';
      return true;
    }
    const lengthHeader = headers['content-length'];
    if (lengthHeader !== undefined) {
      const length = parseContentLength(lengthHeader);
      if (length === -1) {
        this.fail(400);
        return false;
      }
      if (length === -2) {
        this.fail(413);
        return false;
      }
      if (length > 0) {
        this.bodyLength = length;
        this.phase = 'body';
        return true;
      }
    }
    this.dispatch(EMPTY_BODY);
    return true;
  }

  /** Parse one chunk-size line. Returns false when more bytes are needed. */
  private parseChunkSizeLine(): boolean {
    const end = this.pending.indexOf('\r\n');
    if (end === -1) {
      if (this.pending.length > MAX_HEAD_BYTES) this.fail(400);
      return false;
    }
    const size = parseChunkSize(this.pending.subarray(0, end).toString('latin1'));
    this.pending = this.pending.subarray(end + 2);
    if (size === -1) {
      this.fail(400);
      return false;
    }
    if (size === -2 || this.bodyBytes + size > MAX_BODY_BYTES) {
      this.fail(413);
      return false;
    }
    if (size === 0) {
      this.phase = 'trailer';
      return true;
    }
    this.chunkRemaining = size;
    this.phase = 'chunk-data';
    return true;
  }

  /** Parse one chunk's data plus its trailing CRLF. Returns false when more bytes are needed. */
  private parseChunkData(): boolean {
    if (this.pending.length < this.chunkRemaining + 2) return false;
    this.bodyParts.push(this.pending.subarray(0, this.chunkRemaining));
    this.bodyBytes += this.chunkRemaining;
    this.pending = this.pending.subarray(this.chunkRemaining + 2);
    this.phase = 'chunk-size';
    return true;
  }

  /**
   * Parse the trailer section after the final 0-chunk: an empty line on its own (no trailers),
   * or trailer lines ending in an empty line. On success the chunked body is complete.
   */
  private parseTrailers(): boolean {
    if (this.pending.length >= 2 && this.pending[0] === 13 && this.pending[1] === 10) {
      this.pending = this.pending.subarray(2);
      this.finishChunked();
      return true;
    }
    const end = this.pending.indexOf('\r\n\r\n');
    if (end === -1) {
      if (this.pending.length > MAX_HEAD_BYTES) this.fail(400);
      return false;
    }
    this.pending = this.pending.subarray(end + 4);
    this.finishChunked();
    return true;
  }

  private finishChunked(): void {
    const body = this.bodyParts.length === 0 ? EMPTY_BODY : Buffer.concat(this.bodyParts, this.bodyBytes);
    this.bodyParts = [];
    this.phase = 'head';
    this.dispatch(body);
  }

  /** Run the handler and write its reply. A handler error is a 500; the socket stays open. */
  private dispatch(body: Buffer): void {
    const req: HttpRequest = {
      method: this.method,
      target: this.target,
      version: this.version,
      headers: this.headers,
      body,
    };
    let res: HttpResponse;
    try {
      res = this.handler(req);
    } catch (err) {
      process.stderr.write(`[http] handler threw: ${err instanceof Error ? err.stack ?? err.message : String(err)}\n`);
      res = { status: 500, headers: { 'content-type': JSON_TYPE }, body: SERVER_ERROR_BODY };
    }
    this.respond(req, res);
  }

  /** Write one response: headers and body in one corked flush, then close if asked. */
  private respond(req: HttpRequest, res: HttpResponse): void {
    const body = res.body === undefined || res.body === null ? EMPTY_BODY : typeof res.body === 'string' ? Buffer.from(res.body) : res.body;
    const isHead = req.method === 'HEAD';
    const noBody = res.status === 204 || res.status === 304;
    const close = res.close === true || !this.keepAlive;
    let head = `HTTP/${req.version} ${res.status} ${REASONS[res.status] ?? 'OK'}\r\n`;
    if (res.headers !== undefined) {
      for (const name of Object.keys(res.headers)) head += `${name}: ${res.headers[name]}\r\n`;
    }
    if (!noBody) head += `content-length: ${body.length}\r\n`;
    head += `x-github-request-id: ${nextRequestId()}\r\n`;
    if (close) head += 'connection: close\r\n';
    else if (req.version === '1.0') head += 'connection: keep-alive\r\n';
    head += '\r\n';
    this.socket.cork();
    this.socket.write(head);
    if (!isHead && !noBody && body.length > 0) this.socket.write(body);
    this.socket.uncork();
    if (close) {
      this.closed = true;
      this.socket.end();
    }
  }

  /** Send an error reply and close the socket (a malformed request poisons the stream). */
  private fail(status: 400 | 413): void {
    const body = status === 400 ? BAD_REQUEST_BODY : TOO_LARGE_BODY;
    const head =
      `HTTP/${this.version} ${status} ${REASONS[status]}\r\n` +
      `content-type: ${JSON_TYPE}\r\n` +
      `content-length: ${body.length}\r\n` +
      `x-github-request-id: ${nextRequestId()}\r\n` +
      'connection: close\r\n\r\n';
    this.closed = true;
    this.socket.cork();
    this.socket.write(head);
    this.socket.write(body);
    this.socket.uncork();
    this.socket.end();
  }
}

/** Create the TCP server. It speaks HTTP/1.1 and calls `handler` for every request. */
export function createHttpServer(handler: HttpHandler): Server {
  return createServer((socket) => {
    new Connection(socket, handler);
  });
}
