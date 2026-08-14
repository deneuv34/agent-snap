export interface CoalescedScheduler {
  trigger(key: string, run: () => Promise<void>): void
  cancel(key: string): void
  flush(): Promise<void>
  dispose(): void
}

interface Entry {
  timer: NodeJS.Timeout
  run: () => Promise<void>
}

/**
 * Keyed trailing-edge debouncer. Bursts of triggers for the same key collapse
 * into a single run once the key has been quiet for `delayMs`. `cancel`
 * drops a pending trigger without running it. `flush` runs every still
 * pending trigger immediately (used before shutdown) and waits for in-flight
 * runs. After `dispose`, the scheduler is terminal: triggers are ignored and
 * pending timers are cancelled.
 */
export function createCoalescedScheduler(delayMs: number): CoalescedScheduler {
  const entries = new Map<string, Entry>()
  const inflight = new Map<string, Promise<void>>()
  let disposed = false

  function exec(key: string, run: () => Promise<void>): void {
    if (disposed) return
    const previous = inflight.get(key) ?? Promise.resolve()
    const next = previous
      .catch(() => {})
      .then(() => run())
      .catch(() => {})
    inflight.set(key, next)
    void next.finally(() => {
      if (inflight.get(key) === next) inflight.delete(key)
    })
  }

  return {
    trigger(key, run) {
      if (disposed) return
      const existing = entries.get(key)
      if (existing !== undefined) clearTimeout(existing.timer)
      entries.set(key, {
        timer: setTimeout(() => {
          entries.delete(key)
          exec(key, run)
        }, delayMs),
        run,
      })
    },
    cancel(key) {
      const existing = entries.get(key)
      if (existing === undefined) return
      clearTimeout(existing.timer)
      entries.delete(key)
    },
    async flush() {
      for (const [key, entry] of [...entries]) {
        clearTimeout(entry.timer)
        entries.delete(key)
        exec(key, entry.run)
      }
      await Promise.allSettled([...inflight.values()])
    },
    dispose() {
      disposed = true
      for (const entry of entries.values()) clearTimeout(entry.timer)
      entries.clear()
    },
  }
}
