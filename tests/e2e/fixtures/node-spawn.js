const { spawnSync } = require('child_process');
const command = process.argv[2];
const args = process.argv.slice(3);
const result = spawnSync(command, args, { stdio: 'inherit' });
if (result.error) {
  console.error(`node-spawn: ${result.error.code}: ${result.error.message}`);
  process.exit(111);
}
if (result.signal) process.kill(process.pid, result.signal);
process.exit(result.status);
