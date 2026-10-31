// Loads every page and tab of duva's UI against cmd/uipreview and fails if
// one does not mount: a page error, a console error, or template text
// ("{{ ... }}") left on the page because petite-vue never took over. Unit
// tests do not run the page in a browser, which is how a template petite-vue
// could not place shipped in 5.7.5 and 5.7.6.
//
// Run by `make test-ui`. Add a view to VIEWS when the page gets one.
import { spawn, execFileSync } from "node:child_process"
import { mkdtempSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { chromium } from "playwright"

const addr = process.env.UIPREVIEW_ADDR || "127.0.0.1:8099"
const base = "http://" + addr

// The preview has no sign-in server, so this one request 404s there.
const expected = (text) => text.includes("auth/client.js") || text.includes("status of 404")

// Each view: a name, the hash to load, and what to do once there.
const VIEWS = [
  { name: "queue", hash: "", expect: async (p) => p.locator("#sec-queue").waitFor() },
  // The preview has a service with two entries in each: the second must say
  // "then", not its name again.
  { name: "automatic", hash: "", go: "Automatic", expect: async (p) => {
      await p.locator("#sec-scheduled").waitFor()
      await p.locator("#sec-scheduled .svc.then").first().waitFor()
  } },
  { name: "deferred", hash: "", go: "Deferred", expect: async (p) => {
      await p.locator("#sec-upcoming").waitFor()
      await p.locator("#sec-upcoming .svc.then").first().waitFor()
  } },
  { name: "jobs", hash: "#jobs", expect: async (p) => {
      await p.locator(".jobspage .group").first().waitFor()
      const groups = await p.locator(".jobspage .group").count()
      if (groups < 1) throw new Error("no job groups")
      // A phone shows only each job's own row until it is tapped; a wide
      // screen always shows the hosts.
      const rows = p.locator(".jobspage .jobrows li").first()
      const phone = p.viewportSize().width <= 720
      if ((await rows.isVisible()) === phone) throw new Error(phone ? "hosts shown before the job is tapped" : "hosts hidden on a wide screen")
      if (phone) {
        await p.locator(".jobspage .jobgroup").first().click()
        await rows.waitFor()
      }
  } },
]
const WIDTHS = [{ label: "desktop", width: 1600, height: 900 }, { label: "phone", width: 390, height: 844 }]

// Built, then run directly: killing `go run` leaves the binary it started
// holding the port, and the next run would test whatever that one served.
const module = new URL("../../duva-v5/", import.meta.url).pathname
const bin = join(mkdtempSync(join(tmpdir(), "uipreview-")), "uipreview")
execFileSync("go", ["build", "-o", bin, "./cmd/uipreview"], { cwd: module, stdio: "inherit" })
const server = spawn(bin, [], { env: { ...process.env, UIPREVIEW_ADDR: addr }, stdio: "ignore" })
const stop = () => server.kill()
process.on("exit", stop)

async function up() {
  for (let i = 0; i < 120; i++) {
    try { if ((await fetch(base + "/healthz")).status < 500) return } catch {}
    await new Promise((r) => setTimeout(r, 500))
  }
  throw new Error("the preview did not start on " + addr)
}

let failed = 0
try {
  await up()
  const browser = await chromium.launch()
  for (const w of WIDTHS) for (const v of VIEWS) {
    const page = await browser.newPage({ viewport: { width: w.width, height: w.height } })
    const problems = []
    page.on("pageerror", (e) => problems.push("page error: " + e.message))
    page.on("console", (m) => { if (m.type() === "error" && !expected(m.text())) problems.push("console: " + m.text()) })
    try {
      await page.goto(base + "/" + v.hash)
      if (v.go) {
        // The margin menu on a wide screen, the tab bar on a narrow one.
        await page.locator(w.width >= 1440 ? "nav.side button" : "nav.tabbar button", { hasText: v.go }).click()
      }
      await v.expect(page)
      await page.waitForTimeout(500)
      const raw = await page.locator("text=/\\{\\{/").count()
      if (raw) problems.push(raw + " element(s) still show {{ }}: the page did not mount")
    } catch (e) {
      problems.push(e.message.split("\n")[0])
    }
    const ok = problems.length === 0
    console.log((ok ? "ok   " : "FAIL ") + w.label + " " + v.name + (ok ? "" : "\n       " + problems.join("\n       ")))
    if (!ok) failed++
    await page.close()
  }

  // An apply, last, because the preview plays it for real: when it finishes
  // the service's entries are gone, as they are in the queue, and a view
  // checked after it would find nothing to look at. Desktop width only: what
  // is checked is which row the page puts progress on, not how it lays out.
  const problems = []
  const page = await browser.newPage({ viewport: { width: 1600, height: 900 } })
  try {
    await page.goto(base + "/")
    await page.locator("nav.side button", { hasText: "Automatic" }).click()
    // Applying the last of a service's entries shows progress on that row
    // alone: the rows before it are the same service, but they are not what
    // is being applied. Armed by the first press, applied by the second.
    // Direct children only: a row's progress lines are <li>s of their own.
    const all = await page.locator("#sec-scheduled .group").first().locator(".rows > li").all()
    const then = []
    for (const [i, li] of all.entries()) if (await li.locator(".svc.then").count()) then.push(i)
    const last = then[then.length - 1]
    const first = then[0] - 1 // the row that carries the service's name
    await all[last].locator("button.apply").click()
    await all[last].locator("button.apply").click()
    await all[last].locator(".steps").waitFor()
    for (const [i, li] of all.entries()) {
      if (i === last) continue
      if (await li.locator(".steps").count()) problems.push("progress shown on a row that is not being applied")
      const button = li.locator("button.apply").first()
      if ((await button.innerText()).includes("Updating")) problems.push("a row that is not being applied says Updating")
      // The decider takes one apply per host at a time, so the service's
      // other entries must not be pressable while one is in flight.
      if (i >= first && i < last && !(await button.isDisabled())) problems.push("another entry of the service being applied can be pressed")
    }
    // And when it finishes, the whole of the service goes, not only that row.
    await page.locator("#sec-scheduled .svc.then").first().waitFor({ state: "detached", timeout: 15000 })
  } catch (e) {
    problems.push(e.message.split("\n")[0])
  }
  const ok = problems.length === 0
  console.log((ok ? "ok   " : "FAIL ") + "desktop apply" + (ok ? "" : "\n       " + [...new Set(problems)].join("\n       ")))
  if (!ok) failed++
  await page.close()
  await browser.close()
} catch (e) {
  console.log("FAIL " + e.message)
  failed++
} finally {
  stop()
}
process.exit(failed ? 1 : 0)
