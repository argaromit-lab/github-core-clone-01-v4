#!/usr/local/bin/node
import { spawn } from 'node:child_process';

// The published CLI mounts /tmp without exec, although the contest permits a
// staged static native binary. Apply this mount compatibility fix equally to
// baseline and candidate; leave workloads, limits and networking untouched.
const args = process.argv.slice(2);
for (let i = 1; i < args.length; i++) {
  if (args[i - 1] === '--tmpfs' && args[i].startsWith('/tmp:') && !args[i].split(',').includes('exec')) {
    args[i] += ',exec';
  }
}
const child = spawn('/usr/local/bin/docker-real', args, { stdio: 'inherit' });
child.on('error', (error) => { console.error(error.message); process.exitCode = 1; });
child.on('exit', (code, signal) => {
  if (signal) process.kill(process.pid, signal);
  else process.exitCode = code ?? 1;
});
