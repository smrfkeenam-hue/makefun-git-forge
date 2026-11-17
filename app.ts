// The local-trial facade: createApp() returns a server-like object whose listen() spawns the Go
// core (`forge --port <port>`) and resolves once it prints READY. The trial harness drives it the
// way it drove the Node server: createApp(), once('error'), listen(port, host, cb).
//
// The child must never outlive this process: the harness ends a trial with SIGTERM, which the
// launcher turns into process.exit(0) — and a plain 'exit' handler that kills the child synchronously
// is the only thing that runs on that path, so it is registered here (a spawned child orphaned by
// an exit would keep the trial port bound and poison the next run).

import { spawn, type ChildProcess } from 'node:child_process';
import { EventEmitter } from 'node:events';
import { accessSync, chmodSync, constants, copyFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

/**
 * Resolve an executable path for the Go core. A binary staged from the repo may
 * lose its +x bit (web uploads commit it 100644), and spawn would fail with
 * EACCES — so chmod it in place when the filesystem allows it, otherwise copy
 * it to a temp file and chmod that.
 */
function resolveCoreBinary(): string {
  const binary = fileURLToPath(new URL('./forge', import.meta.url));
  try {
    chmodSync(binary, 0o755);
  } catch {
    // Read-only checkout: fall through to the /tmp copy below.
  }
  try {
    accessSync(binary, constants.X_OK);
    return binary;
  } catch {
    const tmpBin = join(tmpdir(), `forge-${process.pid}`);
    copyFileSync(binary, tmpBin);
    chmodSync(tmpBin, 0o755);
    return tmpBin;
  }
}

class GoServer extends EventEmitter {
  private child: ChildProcess | null = null;
  private boundPort = 0;

  listen(port: number, _host: string, cb?: () => void): this {
    const binary = resolveCoreBinary();
    const child = spawn(binary, ['--port', String(port)], { stdio: ['ignore', 'pipe', 'pipe'] });
    this.child = child;
    let settled = false;
    let buffered = '';
    child.once('error', (err) => {
      if (settled) return;
      settled = true;
      this.emit('error', err);
    });
    child.once('exit', (code, signal) => {
      if (settled) return;
      settled = true;
      this.emit('error', new Error(`the Go core exited before READY (code ${code}, signal ${signal})`));
    });
    child.stderr?.on('data', (chunk: Buffer) => process.stderr.write(chunk));
    child.stdout?.on('data', (chunk: Buffer) => {
      if (settled) {
        // After READY the core prints nothing; forward anything unexpected.
        process.stdout.write(chunk);
        return;
      }
      buffered += chunk.toString('utf8');
      const newline = buffered.indexOf('\n');
      if (newline === -1) return;
      const line = buffered.slice(0, newline).trim();
      const match = /^READY (\d+)$/.exec(line);
      if (!match) {
        settled = true;
        child.kill('SIGTERM');
        this.emit('error', new Error(`the Go core printed an unexpected first line: ${JSON.stringify(line)}`));
        return;
      }
      settled = true;
      this.boundPort = Number(match[1]);
      if (cb) cb();
    });
    // Kill the child synchronously on any exit path (see the header comment).
    process.on('exit', () => {
      try {
        child.kill('SIGTERM');
      } catch {
        // Already gone.
      }
    });
    return this;
  }

  address(): { address: string; family: string; port: number } | null {
    if (!this.boundPort) return null;
    return { address: '127.0.0.1', family: 'IPv4', port: this.boundPort };
  }

  close(cb?: () => void): this {
    try {
      this.child?.kill('SIGTERM');
    } catch {
      // Already gone.
    }
    if (cb) cb();
    return this;
  }
}

/** Create the server facade. The store lives in the Go core, created once at spawn. */
export function createApp(): GoServer {
  return new GoServer();
}
