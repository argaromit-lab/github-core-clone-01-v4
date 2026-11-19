import { chmod, copyFile, access } from 'node:fs/promises';
import { spawn, spawnSync } from 'node:child_process';
import { accessSync, constants, existsSync } from 'node:fs';
import { mkdtemp, rm } from 'node:fs/promises';
import { arch, platform } from 'node:process';
import { fileURLToPath } from 'node:url';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const root = new URL('.', import.meta.url);
const targetArch = arch === 'x64' ? 'amd64' : arch === 'arm64' ? 'arm64' : '';
if (!targetArch) throw new Error(`Unsupported architecture: ${arch}`);

let executable;
let temporaryDirectory = '';
if (platform === 'linux') {
  executable = fileURLToPath(new URL(`bin/forge-linux-${targetArch}`, root));
  try {
    accessSync(executable, constants.X_OK);
  } catch {
    const staged = '/tmp/forge-bin';
    await copyFile(executable, staged);
    await chmod(staged, 0o755);
    executable = staged;
  }
} else if (platform === 'win32') {
  executable = fileURLToPath(new URL(`bin/forge-windows-${targetArch}.exe`, root));
  try {
    await access(executable, constants.X_OK);
  } catch {
    temporaryDirectory = await mkdtemp(join(tmpdir(), 'forge-practice-'));
    executable = join(temporaryDirectory, 'forge.exe');
    const bundledGo = 'G:/tools/go/bin/go.exe';
    const compiler = process.env.GO || (existsSync(bundledGo) ? bundledGo : 'go');
    const build = spawnSync(compiler, ['build', '-trimpath', '-ldflags=-s -w', '-o', executable, './forge-go'], {
      cwd: fileURLToPath(root),
      env: {
        ...process.env,
        CGO_ENABLED: '0',
        GOOS: 'windows',
        GOARCH: targetArch,
        GOCACHE: process.env.GOCACHE || join(tmpdir(), 'forge-go-cache'),
      },
      encoding: 'utf8',
    });
    if (build.error) throw build.error;
    if (build.status !== 0) throw new Error(build.stderr || 'Could not build the Windows practice executable.');
  }
} else {
  throw new Error(`Unsupported platform: ${platform}`);
}

const child = spawn(executable, [], {
  cwd: root,
  env: process.env,
  stdio: 'inherit',
});
child.on('error', (error) => {
  process.stderr.write(`forge launch failed: ${error.message}\n`);
  process.exitCode = 1;
});
child.on('exit', (code, signal) => {
  if (temporaryDirectory) void rm(temporaryDirectory, { recursive: true, force: true });
  if (signal) process.kill(process.pid, signal);
  else process.exitCode = code ?? 1;
});
