import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { chmodSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { dirname, join } from 'node:path'
import test from 'node:test'
import { fileURLToPath } from 'node:url'

const ROOT_DIR = dirname(dirname(fileURLToPath(import.meta.url)))
const ENTRYPOINT_PATH = join(ROOT_DIR, 'docker/server-entrypoint.sh')
const TEST_IMAGE = 'alpine:3.23'

function writeExecutable(path, content) {
  writeFileSync(path, content)
  chmodSync(path, 0o755)
}

function runEntrypoint(backend, configPath = '/app/config.toml', ctrStatus = 1) {
  const tempDir = mkdtempSync(join(tmpdir(), 'memoh-server-entrypoint-'))
  const appDir = join(tempDir, 'app')
  const configDir = join(tempDir, 'config')
  const fakeBinDir = join(tempDir, 'bin')
  const stateDir = join(tempDir, 'state')
  const callsPath = join(stateDir, 'calls.log')

  try {
    mkdirSync(appDir)
    mkdirSync(configDir)
    mkdirSync(fakeBinDir)
    mkdirSync(stateDir)
    const hostConfigPath = configPath === '/app/config.toml'
      ? join(appDir, 'config.toml')
      : join(configDir, 'config.toml')
    const backendConfig = backend === undefined ? '' : `backend = "${backend}"\n`
    writeFileSync(hostConfigPath, `[container]\n${backendConfig}`)
    writeExecutable(join(appDir, 'memoh-server'), `#!/bin/sh
printf 'memoh-server %s\n' "$*" >> "$ENTRYPOINT_TEST_LOG"
`)

    const fakeCommand = `#!/bin/sh
command_name=\${0##*/}
printf '%s %s\n' "$command_name" "$*" >> "$ENTRYPOINT_TEST_LOG"
case "$command_name" in
  containerd)
    trap 'printf "containerd-stopped\\n" >> "$ENTRYPOINT_TEST_LOG"; exit 0' TERM INT
    : > "$ENTRYPOINT_TEST_CONTAINERD_READY"
    while :; do /bin/sleep 0.05; done
    ;;
  ctr)
    [ "$ENTRYPOINT_TEST_CTR_STATUS" -eq 0 ] || exit "$ENTRYPOINT_TEST_CTR_STATUS"
    for _ in 1 2 3 4 5 6 7 8 9 10; do
      [ -f "$ENTRYPOINT_TEST_CONTAINERD_READY" ] && exit 0
      /bin/sleep 0.01
    done
    exit 1
    ;;
  mkdir)
    exec /bin/mkdir "$@"
    ;;
esac
exit 0
`
    for (const command of ['containerd', 'ctr', 'ip', 'iptables', 'mkdir', 'rm', 'sleep', 'sysctl'])
      writeExecutable(join(fakeBinDir, command), fakeCommand)

    const result = spawnSync('docker', [
      'run',
      '--rm',
      '--tmpfs',
      '/sys/fs/cgroup:rw,nosuid,size=64k',
      '--mount',
      `type=bind,src=${ENTRYPOINT_PATH},dst=/entrypoint.sh,readonly`,
      '--mount',
      `type=bind,src=${appDir},dst=/app,readonly`,
      '--mount',
      `type=bind,src=${configDir},dst=/test/config,readonly`,
      '--mount',
      `type=bind,src=${fakeBinDir},dst=/test/bin,readonly`,
      '--mount',
      `type=bind,src=${stateDir},dst=/test/state`,
      '--env',
      'ENTRYPOINT_TEST_LOG=/test/state/calls.log',
      '--env',
      `ENTRYPOINT_TEST_CTR_STATUS=${ctrStatus}`,
      '--env',
      'ENTRYPOINT_TEST_CONTAINERD_READY=/test/state/containerd.ready',
      '--env',
      'PATH=/test/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin',
      ...(configPath === '/app/config.toml' ? [] : ['--env', `CONFIG_PATH=${configPath}`]),
      '--entrypoint',
      '/bin/sh',
      TEST_IMAGE,
      '-c',
      "printf 'cpu memory\\n' > /sys/fs/cgroup/cgroup.controllers; printf '123\\n' > /sys/fs/cgroup/cgroup.procs; exec /bin/sh /entrypoint.sh",
    ], {
      cwd: ROOT_DIR,
      encoding: 'utf8',
      timeout: 10_000,
    })
    const calls = existsSync(callsPath) ? readFileSync(callsPath, 'utf8') : ''
    return { calls, result }
  }
  finally {
    rmSync(tempDir, { recursive: true, force: true })
  }
}

test('docker backend starts the server without embedded containerd setup', () => {
  const { calls, result } = runEntrypoint('docker')

  assert.equal(result.status, 0, result.stderr || result.stdout)
  assert.match(calls, /^memoh-server serve$/m)
  assert.doesNotMatch(calls, /^(?:containerd|ctr|ip|iptables|mkdir|rm|sleep|sysctl)\b/m)
})

test('apple backend from CONFIG_PATH starts the server without embedded containerd setup', () => {
  const { calls, result } = runEntrypoint('apple', '/test/config/config.toml')

  assert.equal(result.status, 0, result.stderr || result.stdout)
  assert.match(calls, /^memoh-server serve$/m)
  assert.doesNotMatch(calls, /^(?:containerd|ctr|ip|iptables|mkdir|rm|sleep|sysctl)\b/m)
})

test('omitted backend preserves embedded containerd setup and cleanup', () => {
  const { calls, result } = runEntrypoint(undefined, '/app/config.toml', 0)

  assert.equal(result.status, 0, result.stderr || result.stdout)
  assert.match(calls, /^ip link delete cni0$/m)
  assert.match(calls, /^rm -rf \/var\/lib\/cni\/networks\/\* \/var\/lib\/cni\/results\/\*$/m)
  assert.match(calls, /^sysctl -w net\.ipv4\.ip_forward=1$/m)
  assert.match(calls, /^iptables -t nat -C POSTROUTING /m)
  assert.match(calls, /^mkdir -p \/sys\/fs\/cgroup\/init$/m)
  assert.match(calls, /^mkdir -p \/run\/containerd$/m)
  assert.match(calls, /^containerd $/m)
  assert.match(calls, /^ctr version$/m)
  assert.match(calls, /^memoh-server serve$/m)
  assert.match(calls, /^containerd-stopped$/m)
})
