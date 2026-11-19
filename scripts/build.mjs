import { mkdir } from 'node:fs/promises';
import { spawnSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { dirname, resolve } from 'node:path';
import { existsSync } from 'node:fs';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const out = resolve(root, 'bin');
await mkdir(out, { recursive: true });

const windowsGo = 'G:/tools/go/bin/go.exe';
const go = process.env.GO || (process.platform === 'win32' && existsSync(windowsGo) ? windowsGo : 'go');
const targets = [
  { os: 'linux', arch: 'amd64', file: 'forge-linux-amd64' },
  { os: 'linux', arch: 'arm64', file: 'forge-linux-arm64' },
];
if (process.argv.includes('--windows')) targets.push({ os: 'windows', arch: 'amd64', file: 'forge-windows-amd64.exe' });

for (const target of targets) {
  const env = { ...process.env, GOOS: target.os, GOARCH: target.arch, CGO_ENABLED: '0' };
  const result = spawnSync(go, ['build', '-trimpath', '-ldflags=-s -w', '-o', resolve(out, target.file), './forge-go'], {
    cwd: root,
    env,
    stdio: 'inherit',
  });
  if (result.error) throw result.error;
  if (result.status !== 0) process.exit(result.status ?? 1);
  process.stdout.write(`built ${target.os}/${target.arch}: ${target.file}\n`);
}
