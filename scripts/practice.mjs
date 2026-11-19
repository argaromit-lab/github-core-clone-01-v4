import { once } from 'node:events';
import { spawn } from 'node:child_process';
import { existsSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, resolve } from 'node:path';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const kit = resolve(process.env.MAKEFUN_KIT || 'G:/kits/github-core-clone-01-kit');
const binary = resolve(root, 'bin/forge-windows-amd64.exe');
if (!existsSync(binary)) throw new Error('Build the local executable first: npm run build-windows');
const token = process.env.MAKEFUN_SHOP_TOKEN || 'practice-token';

const server = spawn(binary, [], {
  cwd: root,
  env: { ...process.env, MAKEFUN_SHOP_TOKEN: token },
  stdio: ['ignore', 'pipe', 'inherit'],
});
let pending = '';
let port = 0;
server.stdout.setEncoding('utf8');
server.stdout.on('data', (part) => {
  pending += part;
  const match = /forge listening on (\d+)/.exec(pending);
  if (match) port = Number(match[1]);
});

try {
  const deadline = Date.now() + 10_000;
  while (!port && Date.now() < deadline) await new Promise((resolve) => setTimeout(resolve, 25));
  if (!port) throw new Error('The forge did not report its ephemeral port.');
  const runner = spawn(process.env.NODE22 || 'node', [
    '--experimental-strip-types',
    resolve(kit, 'sdk/run.ts'),
    '--url', `http://127.0.0.1:${port}`,
    '--token', token,
    '--perf',
    '--json',
  ], { cwd: kit, stdio: 'inherit', env: process.env });
  const [code] = await once(runner, 'exit');
  process.exitCode = code ?? 1;
} finally {
  server.kill();
}
