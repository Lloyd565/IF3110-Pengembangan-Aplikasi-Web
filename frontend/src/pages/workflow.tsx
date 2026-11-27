import { useEffect, useMemo, useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { api, dbApi, dbPath, enc, newKey } from '../api'
import { useDatabase, useSession } from '../main'
import type { ChangeRequest, Execution, Job, JobSummary } from '../types'
import { Action, Empty, ErrorBox, Field, Form, Loading, Notice, Panel, SQL, Status, TableWrap, date, text, useResource } from '../ui'
import { Proposal, WorkflowNotice } from './migrations'

const executionLabels: Record<string, string> = {
  queued: 'Dalam antrean', running: 'Sedang berjalan', success: 'Sukses',
  failed: 'Gagal', cancelled: 'Dibatalkan', partial: 'Selesai sebagian',
  partially_reverted: 'Dibatalkan sebagian', commit_unknown: 'Hasil commit belum pasti',
}

function RequestStatus({ status }: { status: string }) {
  return <Status value={status} label={status === 'queued' ? 'Dikirim ke worker' : undefined} />
}

function RequestExecution({ databaseID, job }: { databaseID: string; job?: JobSummary }) {
  if (!job) return <span className="muted">Belum ada job</span>
  return <><Status value={job.status} label={executionLabels[job.status]} />
    <small><Link to={`${dbPath(databaseID)}/jobs/${enc(job.id)}`}>Job #{job.id}</Link></small>
  </>
}

export function Requests() {
  const db = useDatabase(); const { capabilities } = useSession(); const resource = useResource<ChangeRequest[]>(capabilities.workflow ? `${dbApi(db.id)}/requests` : null)
  return <>
    <div className="section-head"><h2>Review & approval</h2><button className="secondary" type="button" disabled={!capabilities.workflow} onClick={resource.reload}>Muat ulang</button></div>
    <WorkflowNotice /><ErrorBox error={resource.error} retry={resource.reload} />
    {resource.loading && <Loading />}
    {resource.data?.length === 0 ? <Empty>Belum ada pengajuan.</Empty> : resource.data && <Panel title="Pengajuan perubahan">
      <TableWrap><table>
        <thead><tr><th>Migration</th><th>Aksi</th><th>Mode</th><th>Status pengajuan</th><th>Status eksekusi</th><th>Approval</th></tr></thead>
        <tbody>{resource.data.map(x => <tr key={x.id}>
          <td><Link to={`${dbPath(db.id)}/requests/${enc(x.id)}`}>{x.migration_name || x.migration_id}</Link><small>{x.id}</small></td>
          <td>{x.action}</td><td>{x.mode}</td>
          <td><RequestStatus status={x.status} /></td>
          <td><RequestExecution databaseID={db.id} job={x.job} /></td>
          <td>{(x.approvals ?? []).filter(a => a.status === 'approved').length} / {(x.approvals ?? []).length}</td>
        </tr>)}</tbody>
      </table></TableWrap>
    </Panel>}
  </>
}

export function RequestDetail() {
  const db = useDatabase(); const { rid = '' } = useParams(); const navigate = useNavigate(); const { capabilities } = useSession()
  const resource = useResource<ChangeRequest>(capabilities.workflow ? `${dbApi(db.id)}/requests/${enc(rid)}` : null)
  const key = useMemo(() => newKey(), [rid])
  const jobStatus = resource.data?.job?.status
  useEffect(() => {
    if (resource.loading || resource.error || (jobStatus !== 'queued' && jobStatus !== 'running')) return
    const timer = window.setTimeout(resource.reload, 3000)
    return () => window.clearTimeout(timer)
  }, [jobStatus, resource.loading, resource.error, resource.reload])
  if (!capabilities.workflow) return <WorkflowNotice />
  if (resource.loading && !resource.data) return <Loading />
  if (!resource.data) return <ErrorBox error={resource.error} retry={resource.reload} />
  const x = resource.data
  return <>
    <div className="section-head"><h2>Review {x.migration_name || x.migration_id}</h2><button className="secondary" type="button" onClick={resource.reload}>Muat ulang</button></div>
    <ErrorBox error={resource.error} retry={resource.reload} />
    <dl className="facts">
      <div><dt>Status pengajuan</dt><dd><RequestStatus status={x.status} /></dd></div>
      <div><dt>Status eksekusi</dt><dd><RequestExecution databaseID={db.id} job={x.job} /></dd></div>
      <div><dt>Pengajuan</dt><dd>{x.id}</dd></div><div><dt>Revisi</dt><dd>{x.revision_id}</dd></div>
      <div><dt>Aksi</dt><dd>{x.action}</dd></div><div><dt>Mode</dt><dd>{x.mode}{x.mode === 'batch' && ` · ${x.batch_size} row/batch`}</dd></div>
    </dl>
    <Panel title="Validasi dan SQL yang ditinjau">{(x.validation ?? []).map((v, i) => <Notice key={i}>{v}</Notice>)}{x.mode === 'batch' && <p>Cursor: <code>{x.cursor_column}</code> · Cakupan: {x.scope}</p>}<SQL value={x.sql} /><p className="muted small">Persetujuan terikat pada revisi, arah, mode, dan cakupan. Worker memeriksanya kembali sebelum eksekusi.</p></Panel>
    <Panel title="Persetujuan leader group"><TableWrap><table><thead><tr><th>Group</th><th>Leader</th><th>Status</th><th>Komentar</th></tr></thead><tbody>{(x.approvals ?? []).map(a => <tr key={a.group_id}><td>{a.group_name}</td><td>{a.leader_name || 'Belum ditetapkan'}</td><td><Status value={a.status} /></td><td>{a.comment || '—'}</td></tr>)}</tbody></table></TableWrap>{(x.approvals ?? []).length === 0 && <Notice>Daftar leader belum tersedia. Eksekusi tetap menunggu validasi server.</Notice>}{x.can_submit && <Action run={async () => { await api(`${dbApi(db.id)}/requests/${enc(rid)}/submit`, { method: 'POST', body: {} }); resource.reload() }}>Kirim untuk review</Action>}{x.can_approve && <Form onSubmit={async d => { await api(`${dbApi(db.id)}/requests/${enc(rid)}/review`, { method: 'POST', body: { decision: text(d, 'decision'), comment: text(d, 'comment') } }); resource.reload() }}><Field label="Komentar review"><textarea name="comment" rows={3} maxLength={4000} /></Field><div className="actions"><button name="decision" value="approve">Setujui perubahan</button><button name="decision" value="reject" className="danger">Tolak perubahan</button></div></Form>}</Panel>
    {x.can_enqueue && db.level === 'edit' && <Panel title="Kirim ke queue"><p>Pengiriman mengembalikan job ID. Keberhasilan SQL dipantau pada halaman job.</p><Action confirm="Masukkan perubahan yang disetujui ini ke queue?" run={async () => { const job = await api<Job>(`${dbApi(db.id)}/requests/${enc(rid)}/enqueue`, { method: 'POST', body: {}, key }); navigate(`${dbPath(db.id)}/jobs/${enc(job.id)}`) }}>Masukkan ke queue</Action></Panel>}
  </>
}

export function History() {
  const db = useDatabase(); const { capabilities } = useSession(); const [hide, setHide] = useState(false); const [selected, setSelected] = useState<{ execution: Execution; action: string } | null>(null)
  const resource = useResource<Execution[]>(capabilities.workflow ? `${dbApi(db.id)}/history?hide_rollback=${hide}` : null)
  return <><h2>Execution history</h2><WorkflowNotice /><label className="check"><input type="checkbox" checked={hide} onChange={e => { setHide(e.target.checked); setSelected(null) }} disabled={!capabilities.workflow} /> Hide rollback: sembunyikan pasangan execution yang dibatalkan dan rollback suksesnya</label><p className="muted small">Filter tidak menghapus data. Forward dan attempt gagal tetap terlihat.</p><ErrorBox error={resource.error} retry={resource.reload} />{resource.loading && !resource.data && <Loading />}{resource.data?.length === 0 && <Empty>Belum ada attempt eksekusi.</Empty>}{resource.data?.map(x => <Panel key={x.id} title={x.migration_name || x.migration_id}><div className="section-head"><span>{x.action} · attempt {x.attempt}</span><Status value={x.status} /></div><dl className="facts"><div><dt>Execution ID</dt><dd>{x.id}</dd></div><div><dt>Revisi</dt><dd>{x.revision_id}</dd></div><div><dt>Pengguna</dt><dd>{x.actor}</dd></div><div><dt>Waktu</dt><dd>{date(x.at)}</dd></div>{x.reverts_execution_id && <div><dt>Membatalkan</dt><dd>{x.reverts_execution_id}</dd></div>}</dl>{x.error_message && <div className="alert error" role="alert">{x.error_message}</div>}<details><summary>SQL yang benar-benar dijalankan</summary><SQL value={x.sql} /></details>{db.level === 'edit' && <div className="actions">{x.can_rollback && <button className="secondary" type="button" onClick={() => setSelected({ execution: x, action: 'rollback' })}>Ajukan rollback</button>}{x.can_forward && <button className="secondary" type="button" onClick={() => setSelected({ execution: x, action: 'forward' })}>Ajukan forward</button>}</div>}{selected?.execution.id === x.id && <Proposal key={`${x.id}-${selected.action}`} migrationID={x.migration_id} revisionID={x.revision_id} action={selected.action} executionID={x.id} />}</Panel>)}</>
}

const terminal = new Set(['success', 'succeeded', 'failed', 'cancelled', 'partial', 'partially_reverted', 'unknown', 'commit_unknown'])

export function Jobs() {
  const db = useDatabase(); const { capabilities } = useSession(); const resource = useResource<Job[]>(capabilities.jobs ? `${dbApi(db.id)}/jobs` : null)
  useEffect(() => { if (resource.loading || !resource.data?.some(x => !terminal.has(x.status))) return; const timer = window.setTimeout(resource.reload, 3000); return () => clearTimeout(timer) }, [resource.data, resource.loading, resource.reload])
  return <><div className="section-head"><h2>Queue & job</h2><button type="button" className="secondary" onClick={resource.reload} disabled={!capabilities.jobs}>Muat ulang</button></div>{!capabilities.jobs && <Notice>Layanan queue dan worker belum tersedia.</Notice>}<ErrorBox error={resource.error} retry={resource.reload} />{resource.loading && !resource.data && <Loading />}{resource.data?.length === 0 && <Empty>Belum ada job.</Empty>}{resource.data && <Panel title="Pekerjaan database"><TableWrap><table><thead><tr><th>Job ID</th><th>Status</th><th>Row committed</th><th>Batch committed</th><th>Attempt</th><th>Diperbarui</th></tr></thead><tbody>{resource.data.map(j => <tr key={j.id}><td><Link to={`${dbPath(db.id)}/jobs/${enc(j.id)}`}>{j.id}</Link></td><td><Status value={j.status} /></td><td>{j.committed_rows}</td><td>{j.committed_batches}</td><td>{j.attempt}</td><td>{date(j.updated_at)}</td></tr>)}</tbody></table></TableWrap></Panel>}</>
}

export function JobDetail() {
  const db = useDatabase(); const { jid = '' } = useParams(); const { capabilities } = useSession(); const navigate = useNavigate()
  const resource = useResource<Job>(capabilities.jobs ? `${dbApi(db.id)}/jobs/${enc(jid)}` : null)
  const keys = useMemo(() => ({ resume: newKey(), compensate: newKey() }), [jid])
  useEffect(() => { if (resource.loading || !resource.data || terminal.has(resource.data.status)) return; const timer = window.setTimeout(resource.reload, 2000); return () => clearTimeout(timer) }, [resource.data, resource.loading, resource.reload])
  if (!capabilities.jobs) return <Notice>Layanan queue dan worker belum tersedia.</Notice>
  if (resource.loading && !resource.data) return <Loading />
  const job = resource.data
  if (!job) return <ErrorBox error={resource.error} retry={resource.reload} />
  async function recover(action: 'resume' | 'compensate') { const request = await api<ChangeRequest>(`${dbApi(db.id)}/jobs/${enc(jid)}/${action}`, { method: 'POST', body: {}, key: keys[action] }); navigate(`${dbPath(db.id)}/requests/${enc(request.id)}`) }
  const percent = job.total_rows && job.total_rows > 0 ? Math.max(0, Math.min(100, job.committed_rows / job.total_rows * 100)) : undefined
  return <><div className="section-head"><h2>Progress job</h2><Status value={job.status} /></div><ErrorBox error={resource.error} retry={resource.reload} /><Panel title={job.id}><progress aria-label="Progress row committed" value={percent} max={100} /><div className="stat-grid"><div><strong>{job.committed_rows.toLocaleString('id-ID')}</strong><span>row committed{job.total_rows !== null && ` / ${job.total_rows.toLocaleString('id-ID')}`}</span></div><div><strong>{job.committed_batches}</strong><span>batch committed</span></div><div><strong>{job.attempt}</strong><span>attempt</span></div></div><dl className="facts"><div><dt>Execution</dt><dd>{job.execution_id || 'Menunggu worker'}</dd></div><div><dt>Diperbarui</dt><dd>{date(job.updated_at)}</dd></div></dl>{job.last_error && <div className="alert error" role="alert">{job.last_error}</div>}{['partial', 'partially_reverted', 'unknown', 'commit_unknown'].includes(job.status) && <Notice>Database membutuhkan pemulihan atau pemeriksaan hasil commit. Batch committed tidak dianggap batal secara otomatis.</Notice>}<div className="actions">{job.request_id && <Link to={`${dbPath(db.id)}/requests/${enc(job.request_id)}`}>Lihat pengajuan</Link>}<button type="button" className="secondary" onClick={resource.reload}>Muat ulang</button></div></Panel>{db.level === 'edit' && (job.can_resume || job.can_compensate) && <Panel title="Pengajuan pemulihan"><p>Pemulihan melewati validasi dan approval. Resume melanjutkan checkpoint; kompensasi membatalkan batch committed dalam urutan terbalik.</p><div className="actions">{job.can_resume && <Action run={() => recover('resume')}>Ajukan resume</Action>}{job.can_compensate && <Action danger confirm="Ajukan kompensasi untuk batch committed?" run={() => recover('compensate')}>Ajukan kompensasi</Action>}</div></Panel>}</>
}
