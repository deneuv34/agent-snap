import { test } from "node:test"
import assert from "node:assert/strict"
import { createCoalescedScheduler } from "../src/index.js"

const sleep = (ms: number) => new Promise((resolve) => setTimeout(resolve, ms))

test("bursts for the same key collapse into one run", async () => {
  const scheduler = createCoalescedScheduler(30)
  let runs = 0
  scheduler.trigger("repo", () => {
    runs += 1
    return Promise.resolve()
  })
  scheduler.trigger("repo", () => {
    runs += 1
    return Promise.resolve()
  })
  scheduler.trigger("repo", () => {
    runs += 1
    return Promise.resolve()
  })
  await sleep(80)
  assert.equal(runs, 1)
  scheduler.dispose()
})

test("different keys run independently", async () => {
  const scheduler = createCoalescedScheduler(30)
  const seen: string[] = []
  scheduler.trigger("a", () => {
    seen.push("a")
    return Promise.resolve()
  })
  scheduler.trigger("b", () => {
    seen.push("b")
    return Promise.resolve()
  })
  await sleep(80)
  assert.deepEqual(seen.sort(), ["a", "b"])
  scheduler.dispose()
})

test("flush runs pending triggers immediately and waits for in-flight work", async () => {
  const scheduler = createCoalescedScheduler(10_000)
  let runs = 0
  scheduler.trigger("repo", async () => {
    await sleep(20)
    runs += 1
  })
  await scheduler.flush()
  assert.equal(runs, 1)
  scheduler.dispose()
})

test("dispose cancels pending triggers", async () => {
  const scheduler = createCoalescedScheduler(30)
  let runs = 0
  scheduler.trigger("repo", () => {
    runs += 1
    return Promise.resolve()
  })
  scheduler.dispose()
  await sleep(80)
  assert.equal(runs, 0)
})

test("cancel drops a pending trigger without running it", async () => {
  const scheduler = createCoalescedScheduler(30)
  let runs = 0
  scheduler.trigger("repo", () => {
    runs += 1
    return Promise.resolve()
  })
  scheduler.cancel("repo")
  await sleep(80)
  assert.equal(runs, 0)
  scheduler.dispose()
})

test("cancel of a non-pending key is a no-op", async () => {
  const scheduler = createCoalescedScheduler(30)
  let runs = 0
  scheduler.trigger("repo", () => {
    runs += 1
    return Promise.resolve()
  })
  scheduler.cancel("other")
  await sleep(80)
  assert.equal(runs, 1)
  scheduler.dispose()
})

test("scheduler is terminal after dispose", async () => {
  const scheduler = createCoalescedScheduler(30)
  scheduler.dispose()
  let runs = 0
  scheduler.trigger("repo", () => {
    runs += 1
    return Promise.resolve()
  })
  await scheduler.flush()
  assert.equal(runs, 0)
})
