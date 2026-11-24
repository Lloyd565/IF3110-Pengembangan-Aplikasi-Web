import { useEffect, useState } from 'react'
import { Link, useNavigate, useParams, useSearchParams } from 'react-router-dom'
import { api, dbApi, dbPath, enc } from '../api'
import { useDatabase, useSession } from '../main'
import type { ChangeInput, ChangeRequest, Draft, Migration } from '../types'
import { Empty, ErrorBox, Field, Form, Loading, Notice, Panel, SQL, Status, TableWrap, date, number, text, useResource } from '../ui'

export function WorkflowNotice() { return !useSession().capabilities.workflow ? <Notice>Workflow migration belum tersedia. Peninjauan dan eksekusi dinonaktifkan sementara.</Notice> : null }

export function MigrationList() {
  const db = useDatabase(); const { capabilities } = useSession(); const resource = useResource<Migration[]>(`${dbApi(db.id)}/migrations`); const [search] = useSearchParams()
  const generated = search.get('generated')?.split(',') ?? []
  return <><div className="section-head"><h2>Migration</h2>{db.level === 'edit' && capabilities.workflow && <Link className="button" to={`${dbPath(db.id)}/migrations/new`}>Draft baru</Link>}</div><WorkflowNotice />{generated.length > 1 && <Panel title="Urutan draft yang dihasilkan"><p>Review dan edit setiap draft, lalu ajukan sesuai urutan dependensi.</p><ol>{generated.map(id => resource.data?.find(x => x.id === id)).filter((x): x is Migration => !!x).map(m => <li key={m.id}><Link to={`${dbPath(db.id)}/migrations/${enc(m.id)}/edit`}>{m.name}</Link></li>)}</ol></Panel>}<ErrorBox error={resource.error} retry={resource.reload} />{resource.loading && <Loading />}{resource.data?.length === 0 ? <Empty>Belum ada migration.</Empty> : <Panel title="Definisi migration"><TableWrap><table><thead><tr><th>Nama</th><th>Versi/revisi</th><th>Status</th><th>Pembuat</th><th>Dibuat</th></tr></thead><tbody>{resource.data?.map(m => <tr key={m.id}><td><Link to={`${dbPath(db.id)}/migrations/${enc(m.id)}`}>{m.name}</Link></td><td>{m.version ?? 'Draft'}<small>{m.revision_id}</small></td><td><Status value={m.status} /></td><td>{m.created_by}</td><td>{date(m.created_at)}</td></tr>)}</tbody></table></TableWrap></Panel>}</>
}

type Plan = { Analyze: boolean; Statements: { N: number; SQL: string; Explained: boolean; PlanningTime: number; ExecutionTime: number; Nodes: Record<string, unknown>[] }[] }
function ExplainPlan({ plan }: { plan: Plan }) {
  const columns = ['Node Type', 'Relation Name', 'Index Name', 'Join Type', 'Strategy', 'Total Cost', 'Plan Rows', ...(plan.Analyze ? ['Actual Rows', 'Actual Loops', 'Actual Total Time'] : [])]
  return <Panel title={plan.Analyze ? 'Hasil EXPLAIN ANALYZE' : 'Hasil EXPLAIN'}>{plan.Statements.map(s => <section className="definition" key={s.N}><h3>Statement {s.N}</h3><SQL value={s.SQL} />{s.Explained ? <><p className="muted">Planning: {s.PlanningTime} ms · Execution: {s.ExecutionTime} ms</p><TableWrap><table><thead><tr><th>Depth</th>{columns.map(c => <th key={c}>{c}</th>)}<th>Condition / filter</th></tr></thead><tbody>{s.Nodes.map((n, i) => <tr key={i}><td>{String(n.Depth ?? 0)}</td>{columns.map(c => <td key={c}>{String(n[c] ?? '—')}</td>)}<td>{['Index Cond', 'Hash Cond', 'Join Filter', 'Filter'].filter(k => n[k]).map(k => `${k}: ${String(n[k])}`).join('; ') || '—'}</td></tr>)}</tbody></table></TableWrap></> : <p className="muted">DDL atau statement pendukung; tidak memiliki plan query. Transaksi analisis dibatalkan.</p>}</section>)}</Panel>
}

export function MigrationEditor() {
  const db = useDatabase(); const { capabilities } = useSession(); const { mid } = useParams(); const [search] = useSearchParams(); const navigate = useNavigate(); const from = search.get('from'); const readID = mid ?? from
  const resource = useResource<Migration>(readID ? `${dbApi(db.id)}/migrations/${enc(readID)}` : null)
  const [irreversible, setIrreversible] = useState(false); const [plan, setPlan] = useState<Plan | null>(null)
  useEffect(() => { setIrreversible(resource.data?.irreversible ?? false); setPlan(null) }, [resource.data])
  if (db.level !== 'edit') return <Notice>Hak Edit diperlukan untuk mengubah draft.</Notice>
  if (readID && resource.loading) return <Loading />
  if (resource.error) return <ErrorBox error={resource.error} retry={resource.reload} />
  if (mid && resource.data && !resource.data.can_edit) return <Notice>Definisi ini tidak dapat diubah. Buat migration baru untuk perbaikan.</Notice>
  const m = resource.data
  return <><h2>{mid ? 'Ubah draft' : 'Draft migration baru'}</h2><WorkflowNotice />{from && <Notice>SQL disalin ke definisi baru. Tinjau up/down dan alasan irreversible sebelum menyimpan.</Notice>}<Panel title="Definisi perubahan"><Form key={readID ?? 'new'} onSubmit={async d => {
    if (text(d, 'intent') === 'explain') { setPlan(await api<Plan>(`${dbApi(db.id)}/migrations/${enc(mid ?? 'draft')}/explain`, { method: 'POST', body: { sql: text(d, 'sql_up'), analyze: d.has('analyze') }, timeout: 75000 })); return }
    const draft: Draft = { name: text(d, 'name'), sql_up: text(d, 'sql_up'), sql_down: irreversible ? '' : text(d, 'sql_down'), irreversible, irreversible_reason: irreversible ? text(d, 'irreversible_reason') : '' }
    const saved = await api<Migration>(`${dbApi(db.id)}/migrations${mid ? `/${enc(mid)}` : ''}`, { method: mid ? 'PUT' : 'POST', body: draft }); navigate(`${dbPath(db.id)}/migrations/${enc(saved.id)}`)
  }}><Field label="Nama migration"><input name="name" maxLength={200} defaultValue={m?.name ?? ''} required /></Field><Field label="SQL up"><textarea name="sql_up" className="sql-editor" rows={12} defaultValue={m?.sql_up ?? ''} maxLength={100000} spellCheck={false} required /></Field><label className="check"><input type="checkbox" checked={irreversible} onChange={e => setIrreversible(e.target.checked)} /> Irreversible: perubahan tidak dapat dipulihkan dengan aman</label>{irreversible ? <Field label="Alasan irreversible"><textarea name="irreversible_reason" rows={3} maxLength={4000} defaultValue={m?.irreversible_reason ?? ''} required /></Field> : <Field label="SQL down"><textarea name="sql_down" className="sql-editor" rows={8} defaultValue={m?.sql_down ?? ''} maxLength={100000} spellCheck={false} required /></Field>}<p className="muted">Up/down yang pernah dieksekusi bersifat immutable. Menyimpan draft tidak menjalankan SQL.</p>{db.dbms === 'mariadb' && <Notice>MariaDB: satu DDL per migration atau batch DML InnoDB. DDL melakukan implicit commit.</Notice>}<div className="actions"><button name="intent" value="save" disabled={!capabilities.workflow}>Simpan draft</button><Link to={`${dbPath(db.id)}/migrations`}>Batal</Link></div>{db.dbms === 'postgresql' && <div className="analysis-options"><label className="check"><input name="analyze" type="checkbox" /> EXPLAIN ANALYZE: jalankan query untuk mengukur waktu/row aktual dalam transaksi yang dibatalkan</label><button name="intent" value="explain" className="secondary" formNoValidate>Analisis plan SQL up</button></div>}</Form></Panel>{plan && <ExplainPlan plan={plan} />}</>
}

export function Proposal({ migrationID, revisionID, action = 'apply', executionID }: { migrationID: string; revisionID: string; action?: string; executionID?: string }) {
  const db = useDatabase(); const navigate = useNavigate(); const { capabilities } = useSession(); const [mode, setMode] = useState('normal')
  if (db.level !== 'edit' || !capabilities.workflow) return null
  return <Panel title={`Ajukan ${action}`}><Form onSubmit={async d => {
    const input: ChangeInput = { migration_id: migrationID, revision_id: revisionID, action, target_execution_id: executionID, mode, batch_size: mode === 'batch' ? number(d, 'batch_size') : 0, cursor_column: mode === 'batch' ? text(d, 'cursor_column') : '', scope: text(d, 'scope') }
    const request = await api<ChangeRequest>(`${dbApi(db.id)}/requests`, { method: 'POST', body: input }); navigate(`${dbPath(db.id)}/requests/${enc(request.id)}`)
  }}><Field label="Mode eksekusi"><select value={mode} onChange={e => setMode(e.target.value)}><option value="normal">Normal (asynchronous)</option><option value="batch">Per batch</option></select></Field>{mode === 'batch' && <><div className="form-grid"><Field label="Batch size"><input name="batch_size" type="number" min={1} max={100000} defaultValue={1000} required /></Field><Field label="Kolom cursor unik dan tidak berubah"><input name="cursor_column" defaultValue="id" maxLength={200} required /></Field></div><Field label="Cakupan target batch"><textarea name="scope" rows={3} maxLength={4000} required /></Field><p className="muted small">Query harus mendukung batching. Batch committed tidak dibatalkan oleh kegagalan batch berikutnya.</p></>}<button>Buat pengajuan review</button><p className="muted small">Setiap leader group yang memiliki akses, termasuk View Only, harus menyetujui perubahan sebelum masuk queue.</p></Form></Panel>
}

export function MigrationDetail() {
  const db = useDatabase(); const { mid = '' } = useParams(); const resource = useResource<Migration>(`${dbApi(db.id)}/migrations/${enc(mid)}`); const { capabilities } = useSession()
  if (resource.loading) return <Loading />
  if (resource.error) return <ErrorBox error={resource.error} retry={resource.reload} />
  const m = resource.data; if (!m) return null
  return <><div className="section-head"><h2>{m.name}</h2><Status value={m.status} /></div><WorkflowNotice /><dl className="facts"><div><dt>Migration ID</dt><dd>{m.id}</dd></div><div><dt>Versi/revisi</dt><dd>{m.version ?? 'Draft'} / {m.revision_id || 'Legacy M1'}</dd></div><div><dt>Pembuat</dt><dd>{m.created_by}</dd></div><div><dt>Dibuat</dt><dd>{date(m.created_at)}</dd></div></dl>{m.error_message && <div className="alert error" role="alert">{m.error_message}</div>}<div className="actions">{db.level === 'edit' && capabilities.workflow && <>{m.can_edit && <Link className="button" to={`${dbPath(db.id)}/migrations/${enc(m.id)}/edit`}>Ubah draft</Link>}<Link className="button secondary" to={`${dbPath(db.id)}/migrations/new?from=${enc(m.id)}`}>Salin sebagai migration baru</Link></>}</div><Panel title="SQL up"><SQL value={m.sql_up} /></Panel><Panel title={m.irreversible ? 'Irreversible' : 'SQL down'}>{m.irreversible ? <Notice>{m.irreversible_reason}</Notice> : <SQL value={m.sql_down} />}</Panel><Proposal migrationID={m.id} revisionID={m.revision_id} /></>
}
