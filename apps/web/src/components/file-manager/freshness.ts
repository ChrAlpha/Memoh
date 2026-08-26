import { parentPath } from './utils'

// One tree refresh request. dirs narrows the re-list to the named parent
// directories; null re-lists every expanded directory. background reloads
// stay silent on failure (no toast, no retry) so passive refreshes can't spam
// the user through a workspace restart.
export interface TreeRefreshSignal {
  seq: number
  dirs: string[] | null
  background: boolean
}

export function dirsFromChangedPaths(paths: readonly string[] | null | undefined): string[] | null {
  if (paths == null) return null
  const dirs: string[] = []
  const seen = new Set<string>()
  for (const path of paths) {
    if (!path) continue
    const dir = parentPath(path)
    if (seen.has(dir)) continue
    seen.add(dir)
    dirs.push(dir)
  }
  return dirs
}

export function nodeNeedsRefresh(nodeDir: string, signal: { dirs: string[] | null }): boolean {
  return signal.dirs === null || signal.dirs.includes(nodeDir)
}

export interface SequentialLoader {
  request: (background: boolean) => void
}

// Serializes reloads of one directory listing: a request that arrives while a
// load is in flight collapses into a single trailing rerun (foreground wins),
// so bursts of refresh signals can't fan out into overlapping requests.
export function createSequentialLoader(load: (background: boolean) => Promise<void>): SequentialLoader {
  let inFlight = false
  let pending: boolean | null = null

  function run(background: boolean) {
    inFlight = true
    load(background)
      .catch(() => {})
      .finally(() => {
        inFlight = false
        if (pending !== null) {
          const next = pending
          pending = null
          run(next)
        }
      })
  }

  return {
    request(background: boolean) {
      if (inFlight) {
        pending = pending === null ? background : (pending && background)
        return
      }
      run(background)
    },
  }
}
