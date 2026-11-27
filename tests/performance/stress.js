import http from 'k6/http'
import { check, fail, sleep } from 'k6'
import { Counter, Rate, Trend } from 'k6/metrics'
import execution from 'k6/execution'

const BASE = __ENV.BASE_URL || 'http://localhost:8080'
const ORIGIN = __ENV.APP_ORIGIN || BASE
const DBS = [__ENV.DB_SAME, __ENV.DB_OTHER || __ENV.DB_SAME]
const TERMINAL = ['success', 'succeeded', 'failed', 'cancelled', 'partial', 'partially_reverted', 'commit_unknown', 'unknown']

const jobDuration = new Trend('job_duration_ms', true)
const queueWait = new Trend('job_queue_wait_ms', true)
const jobsDone = new Counter('jobs_completed')
const jobSuccess = new Rate('job_success')
const phaseNames = ['baseline', 'stress3', 'stress6', 'recovery', 'rampdown']

function phase() { return phaseNames[Math.min(4, Math.floor(execution.instance.currentTestRunDuration / 60000))] }

const levels = [
  { duration: '1m', target: 10 },
  { duration: '1m', target: 30 },
  { duration: '1m', target: 60 },
  { duration: '1m', target: 10 },
  { duration: '30s', target: 0 },
]
export const options = {
  scenarios: {
    api: { executor: 'ramping-vus', exec: 'api', startVUs: 1, stages: levels },
    jobs: { executor: 'ramping-vus', exec: 'jobs', startVUs: 1, gracefulRampDown: '3m', gracefulStop: '3m', stages: levels.map(s => ({ ...s, target: Math.ceil(s.target / 5) })) },
  },
  thresholds: {
    'http_req_duration{scenario:api}': ['p(50)<200', 'p(95)<500'],
    'http_req_failed': ['rate<0.01'],
    'job_success': ['rate>0.95'],
    ...Object.fromEntries(phaseNames.flatMap(stage => [
      [`http_req_duration{scenario:api,stage:${stage}}`, ['p(95)<500']],
      [`http_req_failed{stage:${stage}}`, ['rate<0.01']],
      [`http_reqs{stage:${stage}}`, ['count>=0']],
      [`jobs_completed{stage:${stage}}`, ['count>=0']],
      [`job_queue_wait_ms{stage:${stage}}`, ['p(95)>=0']],
      [`job_duration_ms{stage:${stage}}`, ['p(95)>=0']],
    ])),
  },
  summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'max'],
}

function headers(csrf, extra = {}) {
  return { tags: { stage: phase() }, headers: { 'Content-Type': 'application/json', Accept: 'application/json', Origin: ORIGIN, 'X-CSRF-Token': csrf, ...extra } }
}

function data(res, label) {
  if (!check(res, { [`${label} 2xx`]: r => r.status >= 200 && r.status < 300 })) {
    console.error(`${label}: ${res.status} ${res.body}`)
    return null
  }
  return res.json('data')
}

export function setup() {
  if (!__ENV.DB_SAME || !__ENV.USERNAME || !__ENV.PASSWORD) fail('DB_SAME, USERNAME, dan PASSWORD wajib diisi')
  const pre = data(http.get(`${BASE}/api/auth/csrf`), 'csrf')
  const login = data(http.post(`${BASE}/api/auth/login`, JSON.stringify({ username: __ENV.USERNAME, password: __ENV.PASSWORD }), headers(pre.csrf_token)), 'login')
  if (!login) fail('login gagal')
  return { csrf: login.csrf_token, cookies: http.cookieJar().cookiesForURL(BASE) }
}

function session(s) {
  const jar = http.cookieJar()
  for (const [name, values] of Object.entries(s.cookies)) jar.set(BASE, name, values[0])
}

export function api(s) {
  session(s)
  const db = DBS[__ITER % DBS.length]
  const stage = phase()
  http.get(`${BASE}/api/databases`, { tags: { name: 'databases', stage } })
  http.get(`${BASE}/api/db/${db}/migrations`, { tags: { name: 'migrations', stage } })
  http.get(`${BASE}/api/db/${db}/history?hide_rollback=true`, { tags: { name: 'history', stage } })
  http.get(`${BASE}/api/db/${db}/jobs`, { tags: { name: 'jobs', stage } })
  sleep(1)
}

export function jobs(s) {
  session(s)
  const tags = { stage: phase() }
  const failed = () => jobSuccess.add(false, tags)
  const db = DBS[(__VU + __ITER) % DBS.length]
  const base = `${BASE}/api/db/${db}`
  const slice = (__VU * 1000 + __ITER) % 20
  const m = data(http.post(`${base}/migrations`, JSON.stringify({
    name: `k6 vu${__VU} it${__ITER} ${Date.now()}`,
    sql_up: `UPDATE perf.accounts SET balance = balance + 1, updated_at = now() WHERE id % 20 = ${slice}`,
    sql_down: `UPDATE perf.accounts SET balance = balance - 1, updated_at = now() WHERE id % 20 = ${slice}`,
  }), headers(s.csrf)), 'draft')
  if (!m) { failed(); return }
  const req = data(http.post(`${base}/requests`, JSON.stringify({ migration_id: m.id, revision_id: m.revision_id, action: 'apply', mode: 'normal', batch_size: 0 }), headers(s.csrf)), 'propose')
  if (!req) { failed(); return }
  if (!data(http.post(`${base}/requests/${req.id}/submit`, '{}', headers(s.csrf)), 'submit')) { failed(); return }
  if (!data(http.post(`${base}/requests/${req.id}/review`, JSON.stringify({ decision: 'approve', comment: 'k6' }), headers(s.csrf)), 'approve')) { failed(); return }

  const enqueuedAt = Date.now()
  const job = data(http.post(`${base}/requests/${req.id}/enqueue`, '{}', headers(s.csrf, { 'Idempotency-Key': `k6-${req.id}` })), 'enqueue')
  if (!job) { failed(); return }
  let started = false
  for (let i = 0; i < 600; i++) {
    const cur = data(http.get(`${base}/jobs/${job.id}`, { tags: { name: 'job_poll', stage: phase() } }), 'poll')
    if (cur && !started && !['pending', 'queued'].includes(cur.status)) {
      started = true
      queueWait.add(Date.now() - enqueuedAt, tags)
    }
    if (cur && TERMINAL.includes(cur.status)) {
      jobDuration.add(Date.now() - enqueuedAt, tags)
      jobsDone.add(1, tags)
      jobSuccess.add(cur.status === 'success' || cur.status === 'succeeded', tags)
      return
    }
    sleep(1)
  }
  failed()
}

export function handleSummary(summary) {
  return { [__ENV.SUMMARY_PATH || 'tests/performance/results/summary.json']: JSON.stringify(summary, null, 2), stdout: textSummary(summary) }
}

function textSummary(s) {
  const m = s.metrics
  const v = (name, stat) => (m[name] && m[name].values[stat] !== undefined ? m[name].values[stat].toFixed(1) : '-')
  return [
    `API p50=${v('http_req_duration{scenario:api}', 'med')}ms p95=${v('http_req_duration{scenario:api}', 'p(95)')}ms`,
    `request/s=${v('http_reqs', 'rate')} error_rate=${v('http_req_failed', 'rate')}`,
    `queue_wait p50=${v('job_queue_wait_ms', 'med')}ms p95=${v('job_queue_wait_ms', 'p(95)')}ms`,
    `job_duration p50=${v('job_duration_ms', 'med')}ms p95=${v('job_duration_ms', 'p(95)')}ms`,
    `jobs_completed=${v('jobs_completed', 'count')} (${v('jobs_completed', 'rate')}/s) job_success=${v('job_success', 'rate')}`,
    '',
  ].join('\n')
}
