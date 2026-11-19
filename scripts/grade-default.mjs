import { spawn } from 'node:child_process';
import { createWriteStream, existsSync, readFileSync, readdirSync, writeFileSync } from 'node:fs';
import { once } from 'node:events';

// Run inside the CPU/memory-limited Docker-in-Docker container. The official CLI
// keeps its default Docker driver and creates the trial's HTTP services itself.
const args = process.argv.slice(2);
const saveIndex = args.indexOf('--save');
if (saveIndex < 0 || !args[saveIndex + 1]) throw new Error('Pass --save /results/<run>.');
if (args.includes('--driver')) throw new Error('This runner requires the CLI default Docker driver.');
const save = args[saveIndex + 1];
const log = createWriteStream(`${save}.log`);
const samples = new Map();
const cgroup = (name) => {
  try { return readFileSync(`/sys/fs/cgroup/${name}`, 'utf8').trim(); }
  catch { return null; }
};
const before = { cpu: cgroup('cpu.max'), memory: cgroup('memory.max'), memoryEvents: cgroup('memory.events') };
if (before.memory !== '1073741824' || !before.cpu?.startsWith('100000 ')) {
  throw new Error(`Expected 1 CPU/1024 MiB, got ${JSON.stringify(before)}`);
}
const sample = () => {
  for (const name of readdirSync('/proc')) {
    if (!/^\d+$/.test(name)) continue;
    try {
      const status = readFileSync(`/proc/${name}/status`, 'utf8');
      if (!/^Name:\s+forge-/m.test(status)) continue;
      const peak = Number(/^VmHWM:\s+(\d+)/m.exec(status)?.[1] || 0);
      const previous = samples.get(name) || 0;
      samples.set(name, Math.max(previous, peak));
    } catch { /* The process may exit between listing /proc and reading status. */ }
  }
};
const timer = setInterval(sample, 250);
const child = spawn('node', ['/kit/cli/makefun.mjs', 'grade-local', '--kit', '/kit', '--bot', '/bot', '--instances', '3', ...args], {
  stdio: ['ignore', 'pipe', 'pipe'],
});
child.stdout.pipe(log, { end: false });
child.stderr.pipe(log, { end: false });
let code;
try {
  [code] = await once(child, 'close');
} finally {
  clearInterval(timer);
  sample();
  log.end();
}
const resources = {
  driver: 'docker',
  limits: before,
  memoryEventsAfter: cgroup('memory.events'),
  forgePeakRssKiB: Object.fromEntries(samples),
  maxForgeRssKiB: Math.max(0, ...samples.values()),
  maxForgeRssMiB: Math.max(0, ...samples.values()) / 1024,
  exitCode: code,
  summaryWritten: existsSync(`${save}/summary.json`),
};
if (resources.summaryWritten) {
  const summary = JSON.parse(readFileSync(`${save}/summary.json`, 'utf8'));
  const trials = summary.result?.performance?.trials || [];
  resources.conformanceAndMcpPassed = trials.length === 3 && trials.every((trial) =>
    trial.status === 'finished' && trial.scores.feature === 600 && trial.scores.agent === 200 && trial.scores.working === 1);
  resources.replaysMatched = summary.agent?.trials?.length === 3 && summary.agent.trials.every((trial) =>
    trial.replay?.verdict?.ok === true && trial.live?.state_sha256 === trial.replay?.state_sha256);
  resources.scores = summary.result?.performance?.scores || null;
  resources.forgeUnder60MiB = resources.maxForgeRssKiB > 0 && resources.maxForgeRssKiB < 60 * 1024;
  if (!resources.conformanceAndMcpPassed || !resources.replaysMatched || !resources.forgeUnder60MiB) code = 1;
} else {
  code = 1;
}
resources.exitCode = code;
writeFileSync(`${save}.resources.json`, `${JSON.stringify(resources, null, 2)}\n`);
process.stdout.write(`${JSON.stringify(resources)}\n`);
process.exitCode = code ?? 1;
