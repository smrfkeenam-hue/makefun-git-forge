// The entry <-> env lifecycle, as env/src/wire.ts settles it: bind an ephemeral port, tell the env
// the port it bound, then poll until the env says the trial is over and exit 0. Keep this file as it
// is: the suite scores the server the Go core builds, and the lifecycle is the harness's, not the
// contest's.
//
// The server itself is the statically linked Go binary `forge`, vendored in this repository and
// spawned as a child process: Node owns the lifecycle, Go owns the forge. The child binds the
// ephemeral port and prints `READY <port>` on stdout; this file registers that port with the env,
// polls /done, and kills the child on exit.
//
//   1. MAKEFUN_ENV_URL names the env's origin (http://store:8080 in a trial container). Without it
//      there is nothing to talk to, so the program exits 1 at once.
//   2. Spawn ./forge --port 0 and wait for its READY line. Never bind 8080 — the env owns it.
//   3. POST $MAKEFUN_ENV_URL/register {"port": N}. The env records the address the request came from
//      plus N as your base URL and dials it from then on. The first register wins; a 409 means another
//      entry already registered, and this process keeps waiting like any other.
//   4. GET $MAKEFUN_ENV_URL/done until the body is {"done": true}. The env answers at once ({"done":
//      false} while the suite runs), so this is a poll, not a long-held request. A failed or slow poll
//      is retried with a short exponential backoff; the process never exits before the env says done.
//   5. Kill the child and exit 0. The harness then stops the env, which writes the export the scorer
//      reads.

import { spawn, type ChildProcess } from 'node:child_process';
import { accessSync, chmodSync, constants, copyFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

const ENV_URL = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
if (ENV_URL === '') {
  process.stderr.write('[entry] MAKEFUN_ENV_URL is not set: there is no env to register with\n');
  process.exit(1);
}

/** How long one register or done request may take before it is abandoned and retried. */
const REQUEST_TIMEOUT_MS = 30_000;
/** The first wait between retries, and the longest. */
const RETRY_INITIAL_MS = 250;
const RETRY_MAX_MS = 2_000;
/** How long to wait for the Go core's READY line before giving up. */
const READY_TIMEOUT_MS = 30_000;

const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

/** The wait after `attempt` failed requests: exponential from RETRY_INITIAL_MS, capped. */
function retryDelay(attempt: number): number {
  return Math.min(RETRY_MAX_MS, RETRY_INITIAL_MS * 2 ** Math.min(attempt, 8));
}

/**
 * POST the bound port until the env accepts it. 200 is the normal answer; 409 means a first register
 * already won, which is not this process's to fix. 400 and 405 are contract errors and end the program;
 * a network error or another status is retried, since the env may still be starting.
 */
async function register(port: number): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/register`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ port }),
        signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS),
      });
      if (res.status === 200 || res.status === 409) return;
      if (res.status === 400 || res.status === 405) throw new Error(`register answered HTTP ${res.status}: ${await res.text()}`);
    } catch (err) {
      if (err instanceof Error && err.message.startsWith('register answered HTTP ')) throw err;
    }
    await sleep(retryDelay(attempt));
  }
}

/** Poll GET /done until it answers {"done":true}; never return before that. */
async function waitForDone(): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/done`, { signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS) });
      if (res.ok) {
        const body = (await res.json()) as { done?: unknown };
        if (body.done === true) return;
      }
    } catch {
      // The env is not answering yet, or the poll timed out: it is not done, so poll again.
    }
    await sleep(retryDelay(attempt));
  }
}

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

/**
 * Spawn the Go core and wait for its READY line. The child inherits this process's environment
 * (MAKEFUN_SHOP_TOKEN, MAKEFUN_TRIAL_NOW). Its stderr is forwarded so a core failure is visible.
 */
function spawnCore(): Promise<{ child: ChildProcess; port: number }> {
  const binary = resolveCoreBinary();
  return new Promise((resolve, reject) => {
    const child = spawn(binary, ['--port', '0'], { stdio: ['ignore', 'pipe', 'pipe'] });
    let settled = false;
    let buffered = '';
    const timer = setTimeout(() => {
      if (settled) return;
      settled = true;
      child.kill('SIGTERM');
      reject(new Error('the Go core did not print READY within 30 s'));
    }, READY_TIMEOUT_MS);
    child.once('error', (err) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      reject(new Error(`could not spawn the Go core: ${err.message}`));
    });
    child.once('exit', (code, signal) => {
      if (settled) return;
      settled = true;
      clearTimeout(timer);
      reject(new Error(`the Go core exited before READY (code ${code}, signal ${signal})`));
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
        clearTimeout(timer);
        child.kill('SIGTERM');
        reject(new Error(`the Go core printed an unexpected first line: ${JSON.stringify(line)}`));
        return;
      }
      settled = true;
      clearTimeout(timer);
      resolve({ child, port: Number(match[1]) });
    });
  });
}

/** Spawn the core, register its port, then wait for the env: the whole life of the process. */
async function main(): Promise<void> {
  const { child, port } = await spawnCore();
  // If this process is killed or exits, the child must not outlive it.
  const killChild = (): void => {
    try {
      child.kill('SIGTERM');
    } catch {
      // Already gone.
    }
  };
  process.on('exit', killChild);
  process.once('SIGTERM', () => {
    killChild();
    process.exit(0);
  });
  process.once('SIGINT', () => {
    killChild();
    process.exit(0);
  });
  // A core that dies mid-trial cannot serve: exit 1 rather than hang.
  child.once('exit', (code, signal) => {
    process.stderr.write(`[entry] the Go core exited mid-trial (code ${code}, signal ${signal})\n`);
    process.exit(1);
  });

  process.stdout.write(`[entry] listening on port ${port}\n`);
  await register(port);
  process.stderr.write('[entry] registered; waiting for the env to say done\n');

  await waitForDone();
  killChild();
  process.stdout.write('[entry] done\n');
}

try {
  await main();
  process.exit(0);
} catch (err) {
  process.stderr.write(`[entry] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}
