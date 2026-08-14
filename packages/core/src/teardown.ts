export const KILL_GRACE_MS = 2_000
export const GROUP_WAIT_MS = 3_000
export const GROUP_POLL_MS = 25
export const GUARDIAN_WAIT_MS = KILL_GRACE_MS + GROUP_WAIT_MS + 1_000
export const WINDOWS_TREE_WAIT_MS = 10_000

export type GroupState = "live" | "empty" | "error"

export type GroupEmptyAction = "wait" | "done" | "fail"

/**
 * Pure decision function for the POSIX emptiness verification phase.
 *
 * This phase is observation-only: the client never signals a process group
 * here (all group signals are issued by the guardian supervisor, which is a
 * member of the group and therefore cannot observe a recycled PGID — see
 * client.ts). The client only waits for kill(-pgid, 0) to report ESRCH, and
 * fails closed on observation errors or when the window expires while
 * members remain.
 */
export function groupEmptyDecision(input: { state: GroupState; elapsedMs: number }): GroupEmptyAction {
  if (input.state === "empty") return "done"
  if (input.state === "error") return "fail"
  if (input.elapsedMs >= GROUP_WAIT_MS) return "fail"
  return "wait"
}

export type WindowsTreeAction = "wait" | "done" | "fail"

/**
 * Pure decision function for Windows tree cleanup completion.
 *
 * Cleanup succeeds only when BOTH conditions hold: taskkill /T /F reported
 * success AND the root process exited. A taskkill failure always fails
 * closed — even if the root has since exited, its tree may survive it. An
 * expired window without both conditions is a failure, never a success.
 */
export function windowsTreeCompletionDecision(input: {
  taskkillDone: boolean
  taskkillFailed: boolean
  rootExited: boolean
  elapsedMs: number
}): WindowsTreeAction {
  if (input.taskkillFailed) return "fail"
  if (input.taskkillDone && input.rootExited) return "done"
  if (input.elapsedMs >= WINDOWS_TREE_WAIT_MS) return "fail"
  return "wait"
}
