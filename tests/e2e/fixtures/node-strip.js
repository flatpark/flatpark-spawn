// The shape of VS Code's extension host and pty host: a node process
// starts a child node with LD_PRELOAD removed from its environment (VS
// Code's removeDangerousEnvVariables), and the child does the work, so the
// preload library is not there and only shims (PATH) can help.
//   node-strip.js strip MODE ARGS...  run MODE in a child node without LD_PRELOAD
//   node-strip.js spawn CMD ARGS...   child_process.spawnSync CMD, stdio inherited
//   node-strip.js exists NAME         fs.existsSync($FLATPARK_SPAWN_SHIMS/NAME)
//   node-strip.js find NAME ARGS...   as vscode-neovim's findNvim: the first
//                                     PATH entry where NAME exists on disk,
//                                     then run that file
// Every mode first says whether libflatpark-spawn.so is mapped (stderr).
'use strict';
const cp = require('child_process');
const fs = require('fs');
const path = require('path');

const [mode, ...rest] = process.argv.slice(2);
const loaded = fs.readFileSync('/proc/self/maps', 'utf8').includes('/libflatpark-spawn.so');
console.error(`node-strip ${mode}: preload ${loaded ? 'loaded' : 'not loaded'}`);

function finish(r) {
  if (r.error) {
    console.error(`node-strip: ${r.error.code}: ${r.error.message}`);
    process.exit(111);
  }
  if (r.signal) process.kill(process.pid, r.signal);
  process.exit(r.status);
}

if (mode === 'strip') {
  const env = { ...process.env };
  delete env.LD_PRELOAD;
  finish(cp.spawnSync(process.execPath, [__filename, ...rest], { stdio: 'inherit', env }));
} else if (mode === 'spawn') {
  finish(cp.spawnSync(rest[0], rest.slice(1), { stdio: 'inherit' }));
} else if (mode === 'exists') {
  console.log(fs.existsSync(path.join(process.env.FLATPARK_SPAWN_SHIMS || '/nonexistent', rest[0])));
} else if (mode === 'find') {
  const found = (process.env.PATH || '').split(':').filter(Boolean)
    .map((d) => path.join(d, rest[0])).find((p) => fs.existsSync(p));
  if (!found) {
    console.error(`node-strip: ${rest[0]} not found on disk in PATH`);
    process.exit(112);
  }
  finish(cp.spawnSync(found, rest.slice(1), { stdio: 'inherit' }));
} else {
  console.error(`node-strip: unknown mode ${mode}`);
  process.exit(2);
}
