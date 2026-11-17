// A small HTTP/1.1 server on node:net, written for the forge's traffic: keep-alive and pipelined
// requests on one connection, Content-Length and chunked request bodies, Expect: 100-continue, and
// every response written as one buffer in one socket.write (status line, headers and body together).
//
// Requests on a connection are handled one at a time, in order, so pipelined answers can never
// overtake each other. The request body is read whole before the handler runs.

import { createServer as createNetServer, type Server, type Socket } from 'node:net';
import type { Req, Res } from './http.ts';

const MAX_HEAD = 64 * 1024;
const MAX_BODY = 512 * 1024 * 1024;
const CRLFCRLF = Buffer.from('\r\n\r\n', 'latin1');
const CHUNKED_END = Buffer.from('0\r\n\r\n', 'latin1');
const CONTINUE = Buffer.from('HTTP/1.1 100 Continue\r\n\r\n', 'latin1');

const REASONS: Record<number, string> = {
  200: 'OK', 201: 'Created', 202: 'Accepted', 204: 'No Content', 301: 'Moved Permanently', 302: 'Found', 304: 'Not Modified',
  400: 'Bad Request', 401: 'Unauthorized', 403: 'Forbidden', 404: 'Not Found', 405: 'Method Not Allowed', 409: 'Conflict',
  413: 'Payload Too Large', 415: 'Unsupported Media Type', 422: 'Unprocessable Entity', 431: 'Request Header Fields Too Large',
  500: 'Internal Server Error', 501: 'Not Implemented', 503: 'Service Unavailable',
};

export type Handler = (req: Req, res: Res) => void;

interface Head {
  method: string;
  url: string;
  headers: Record<string, string>;
  keepAlive: boolean;
  bodyStart: number;
  length: number; // content-length, or -1 for chunked
}

class Response implements Res {
  headersSent = false;
  private status = 200;
  private headers: Record<string, string | number> = {};
  private readonly conn: Connection;
  private readonly head: boolean;
  private readonly keepAlive: boolean;

  constructor(conn: Connection, isHead: boolean, keepAlive: boolean) {
    this.conn = conn;
    this.head = isHead;
    this.keepAlive = keepAlive;
  }

  writeHead(status: number, headers: Record<string, string | number> = {}): void {
    this.status = status;
    this.headers = headers;
  }

  end(body?: Buffer | string): void {
    if (this.headersSent) return;
    this.headersSent = true;
    const payload = body === undefined ? null : typeof body === 'string' ? Buffer.from(body, 'utf8') : body;
    const length = payload === null ? 0 : payload.length;
    let head = `HTTP/1.1 ${this.status} ${REASONS[this.status] ?? 'Unknown'}\r\n`;
    let sawLength = false;
    for (const name in this.headers) {
      const value = this.headers[name];
      if (value === undefined) continue;
      if (name === 'content-length' || name === 'Content-Length') {
        sawLength = true;
        head += `content-length: ${this.head ? value : length}\r\n`;
        continue;
      }
      head += `${name}: ${value}\r\n`;
    }
    if (!sawLength && this.status !== 204 && this.status !== 304) head += `content-length: ${length}\r\n`;
    if (!this.keepAlive) head += 'connection: close\r\n';
    head += '\r\n';
    const headLength = Buffer.byteLength(head, 'latin1');
    const sendBody = !this.head && payload !== null && length > 0 && this.status !== 204 && this.status !== 304;
    const out = Buffer.allocUnsafe(headLength + (sendBody ? length : 0));
    out.write(head, 0, 'latin1');
    if (sendBody) payload!.copy(out, headLength);
    this.conn.finish(out, this.keepAlive);
  }

  destroy(): void {
    this.headersSent = true;
    this.conn.destroy();
  }
}

class Connection {
  private readonly socket: Socket;
  private readonly handler: Handler;
  private buffer: Buffer = Buffer.alloc(0);
  private pendingChunks: Buffer[] = [];
  private pendingBytes = 0;
  private head: Head | null = null;
  private continued = false;
  private busy = false;
  private closed = false;

  constructor(socket: Socket, handler: Handler) {
    this.socket = socket;
    this.handler = handler;
    socket.setNoDelay(true);
    socket.on('data', (chunk: Buffer) => this.onData(chunk));
    socket.on('error', () => this.destroy());
    socket.on('close', () => {
      this.closed = true;
    });
  }

  private onData(chunk: Buffer): void {
    if (this.buffer.length === 0 && this.pendingChunks.length === 0) this.buffer = chunk;
    else {
      this.pendingChunks.push(chunk);
      this.pendingBytes += chunk.length;
    }
    if (!this.busy) this.pump();
  }

  /** Bytes buffered so far, as one buffer (joined only when a parse step needs them). */
  private available(): number {
    return this.buffer.length + this.pendingBytes;
  }

  private flatten(): Buffer {
    if (this.pendingChunks.length > 0) {
      this.buffer = Buffer.concat([this.buffer, ...this.pendingChunks], this.buffer.length + this.pendingBytes);
      this.pendingChunks = [];
      this.pendingBytes = 0;
    }
    return this.buffer;
  }

  private pump(): void {
    while (!this.busy && !this.closed) {
      if (this.head === null) {
        if (this.available() === 0) return;
        const buf = this.flatten();
        const end = buf.indexOf(CRLFCRLF);
        if (end < 0) {
          if (buf.length > MAX_HEAD) this.fail(431);
          return;
        }
        const head = parseHead(buf, end);
        if (head === null) {
          this.fail(400);
          return;
        }
        if (head.length > MAX_BODY) {
          this.fail(413);
          return;
        }
        this.head = head;
        this.continued = false;
      }
      const head = this.head;
      let body: Buffer | null;
      let consumed: number;
      if (head.length >= 0) {
        if (this.available() < head.bodyStart + head.length) {
          this.maybeContinue(head);
          return;
        }
        const buf = this.flatten();
        body = buf.subarray(head.bodyStart, head.bodyStart + head.length);
        consumed = head.bodyStart + head.length;
      } else {
        // Chunked: try only once the terminating chunk may have arrived (avoids re-scanning a large body).
        const last = this.pendingChunks.length > 0 ? this.pendingChunks[this.pendingChunks.length - 1]! : this.buffer;
        if (this.available() - head.bodyStart > 1024 * 1024 && !endsWith(last, CHUNKED_END) && last.length >= CHUNKED_END.length) {
          this.maybeContinue(head);
          return;
        }
        const buf = this.flatten();
        const decoded = decodeChunked(buf, head.bodyStart);
        if (decoded === 'bad') {
          this.fail(400);
          return;
        }
        if (decoded === null) {
          this.maybeContinue(head);
          return;
        }
        body = decoded.body;
        consumed = decoded.end;
      }
      this.buffer = this.buffer.subarray(consumed);
      this.head = null;
      this.busy = true;
      const req: Req = { method: head.method, url: head.url, headers: head.headers, body, resume: noop };
      const res = new Response(this, head.method === 'HEAD', head.keepAlive);
      try {
        this.handler(req, res);
      } catch {
        if (!res.headersSent) res.end();
      }
    }
  }

  private maybeContinue(head: Head): void {
    if (!this.continued && head.headers.expect?.toLowerCase() === '100-continue') {
      this.continued = true;
      this.socket.write(CONTINUE);
    }
  }

  /** Called by a Response: write its bytes, then serve the next buffered request. */
  finish(out: Buffer, keepAlive: boolean): void {
    if (this.closed) return;
    this.socket.write(out);
    if (!keepAlive) {
      this.closed = true;
      this.socket.end();
      return;
    }
    this.busy = false;
    if (this.available() > 0) this.pump();
  }

  private fail(status: number): void {
    const body = `{"message":"${REASONS[status] ?? 'Bad Request'}"}`;
    this.socket.end(`HTTP/1.1 ${status} ${REASONS[status] ?? 'Bad Request'}\r\ncontent-type: application/json; charset=utf-8\r\ncontent-length: ${body.length}\r\nconnection: close\r\n\r\n${body}`, 'latin1');
    this.closed = true;
  }

  destroy(): void {
    this.closed = true;
    this.socket.destroy();
  }
}

function noop(): void {}

function endsWith(buf: Buffer, tail: Buffer): boolean {
  return buf.length >= tail.length && buf.compare(tail, 0, tail.length, buf.length - tail.length) === 0;
}

function parseHead(buf: Buffer, end: number): Head | null {
  const text = buf.toString('latin1', 0, end);
  let lineEnd = text.indexOf('\r\n');
  if (lineEnd < 0) lineEnd = text.length;
  const sp1 = text.indexOf(' ');
  const sp2 = text.lastIndexOf(' ', lineEnd);
  if (sp1 <= 0 || sp2 <= sp1) return null;
  const method = text.slice(0, sp1);
  const url = text.slice(sp1 + 1, sp2);
  const version = text.slice(sp2 + 1, lineEnd);
  if (version !== 'HTTP/1.1' && version !== 'HTTP/1.0') return null;
  const headers: Record<string, string> = {};
  let at = lineEnd + 2;
  while (at < text.length) {
    let next = text.indexOf('\r\n', at);
    if (next < 0) next = text.length;
    const colon = text.indexOf(':', at);
    if (colon <= at || colon > next) return null;
    const name = text.slice(at, colon).trim().toLowerCase();
    const value = text.slice(colon + 1, next).trim();
    const prior = headers[name];
    headers[name] = prior === undefined ? value : `${prior}, ${value}`;
    at = next + 2;
  }
  const connection = (headers.connection ?? '').toLowerCase();
  const keepAlive = version === 'HTTP/1.1' ? !connection.includes('close') : connection.includes('keep-alive');
  let length = 0;
  const te = headers['transfer-encoding'];
  if (te !== undefined && te.toLowerCase().includes('chunked')) length = -1;
  else if (headers['content-length'] !== undefined) {
    const raw = headers['content-length'];
    if (!/^\d{1,15}$/.test(raw)) return null;
    length = Number(raw);
  }
  return { method, url, headers, keepAlive, bodyStart: end + 4, length };
}

/** Decode a chunked body starting at `start`; null when incomplete, 'bad' when malformed. */
function decodeChunked(buf: Buffer, start: number): { body: Buffer; end: number } | null | 'bad' {
  const parts: Buffer[] = [];
  let at = start;
  let total = 0;
  for (;;) {
    const lineEnd = buf.indexOf('\r\n', at, 'latin1');
    if (lineEnd < 0) return null;
    const sizeText = buf.toString('latin1', at, lineEnd).split(';')[0]!.trim();
    if (!/^[0-9a-fA-F]+$/.test(sizeText)) return 'bad';
    const size = parseInt(sizeText, 16);
    at = lineEnd + 2;
    if (size === 0) {
      // trailers end with an empty line
      for (;;) {
        const trailerEnd = buf.indexOf('\r\n', at, 'latin1');
        if (trailerEnd < 0) return null;
        const empty = trailerEnd === at;
        at = trailerEnd + 2;
        if (empty) return { body: parts.length === 1 ? parts[0]! : Buffer.concat(parts, total), end: at };
      }
    }
    if (at + size + 2 > buf.length) return null;
    parts.push(buf.subarray(at, at + size));
    total += size;
    if (total > MAX_BODY) return 'bad';
    at += size + 2;
  }
}

export interface HttpServer {
  readonly server: Server;
  listen(port: number): Promise<number>;
  close(): Promise<void>;
}

export function createHttpServer(handler: Handler): HttpServer {
  const sockets = new Set<Socket>();
  const server = createNetServer({ noDelay: true }, (socket) => {
    sockets.add(socket);
    socket.on('close', () => sockets.delete(socket));
    new Connection(socket, handler);
  });
  return {
    server,
    listen: (port) =>
      new Promise((resolve, reject) => {
        server.once('error', reject);
        server.listen(port, () => {
          const address = server.address();
          if (address === null || typeof address === 'string') reject(new Error('the forge did not bind a TCP port'));
          else resolve(address.port);
        });
      }),
    close: () =>
      new Promise((resolve) => {
        server.close(() => resolve());
        for (const socket of sockets) socket.destroy();
      }),
  };
}
