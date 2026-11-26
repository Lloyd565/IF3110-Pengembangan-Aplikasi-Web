import { Children, cloneElement, useCallback, useEffect, useId, useRef, useState, type FormEvent, type ReactElement, type ReactNode } from 'react'
import { api, ApiError } from './api'

export function useResource<T>(path: string | null) {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState<Error | null>(null)
  const [loading, setLoading] = useState(!!path)
  const [version, setVersion] = useState(0)
  const previous = useRef<string | null>(null)
  const reload = useCallback(() => setVersion(x => x + 1), [])
  useEffect(() => {
    const controller = new AbortController()
    if (previous.current !== path) { setData(null); previous.current = path }
    setError(null); setLoading(!!path)
    if (path) api<T>(path, { signal: controller.signal }).then(value => { if (!controller.signal.aborted) setData(value) }).catch(e => { if (!controller.signal.aborted) setError(e) }).finally(() => { if (!controller.signal.aborted) setLoading(false) })
    return () => controller.abort()
  }, [path, version])
  return { data: previous.current === path ? data : null, error: previous.current === path ? error : null, loading: previous.current !== path ? !!path : loading, reload }
}

export function ErrorBox({ error, retry }: { error: unknown; retry?: () => void }) {
  if (!error) return null
  return <div role="alert" className="alert error">{error instanceof Error ? error.message : 'Permintaan gagal.'}{error instanceof ApiError && <small>Kode: {error.status || 'jaringan'} · {error.code}</small>}{retry && <button type="button" className="secondary" onClick={retry}>Coba lagi</button>}</div>
}
export function Loading() { return <p className="loading" role="status">Memuat data…</p> }
export function Empty({ children = 'Belum ada data.' }: { children?: ReactNode }) { return <p className="empty">{children}</p> }
export function Notice({ children }: { children: ReactNode }) { return <div className="alert notice" role="status">{children}</div> }
export function Status({ value }: { value: string }) { return <span className={`badge ${['success', 'approved', 'active', 'edit'].includes(value) ? 'good' : ['failed', 'rejected', 'irreversible', 'unknown', 'partially_reverted'].includes(value) ? 'bad' : ''}`}>{value.replaceAll('_', ' ')}</span> }
export function Panel({ title, children }: { title: string; children: ReactNode }) { return <section className="panel"><h2>{title}</h2>{children}</section> }
export function TableWrap({ children }: { children: ReactNode }) { return <div className="table-wrap" tabIndex={0}>{children}</div> }
export function SQL({ value }: { value: string }) { return <pre className="sql" tabIndex={0}><code>{value || '—'}</code></pre> }
export function Field({ label, children }: { label: string; children: ReactNode }) { const generated = useId(); const child = Children.only(children) as ReactElement<{ id?: string }>; const id = child.props.id ?? generated; return <div className="field"><label htmlFor={id}>{label}</label>{cloneElement(child, { id })}</div> }
export function date(value: string) { return value ? new Intl.DateTimeFormat('id-ID', { dateStyle: 'medium', timeStyle: 'short' }).format(new Date(value)) : '—' }

export function Form({ children, onSubmit, reset = false }: { children: ReactNode; onSubmit: (data: FormData) => Promise<void>; reset?: boolean }) {
  const [busy, setBusy] = useState(false)
  const pending = useRef(false)
  const [error, setError] = useState<unknown>(null)
  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault(); if (pending.current) return
    pending.current = true
    const form = event.currentTarget
    const data = new FormData(form)
    const submitter = (event.nativeEvent as SubmitEvent).submitter
    if (submitter instanceof HTMLButtonElement && submitter.name) data.set(submitter.name, submitter.value)
    setBusy(true); setError(null)
    try { await onSubmit(data); if (reset) form.reset() } catch (e) { setError(e) } finally { pending.current = false; setBusy(false) }
  }
  return <form onSubmit={submit}><ErrorBox error={error} /><fieldset disabled={busy}>{children}</fieldset>{busy && <p role="status">Memproses…</p>}</form>
}

export function Action({ children, run, danger = false, confirm, disabled = false }: { children: ReactNode; run: () => Promise<void>; danger?: boolean; confirm?: string; disabled?: boolean }) {
  const [busy, setBusy] = useState(false)
  const pending = useRef(false)
  const [error, setError] = useState<unknown>(null)
  return <span className="action"><button type="button" className={danger ? 'danger' : 'secondary'} disabled={disabled || busy} onClick={async () => {
    if (pending.current || (confirm && !window.confirm(confirm))) return
    pending.current = true
    setBusy(true); setError(null)
    try { await run() } catch (e) { setError(e) } finally { pending.current = false; setBusy(false) }
  }}>{busy ? 'Memproses…' : children}</button><ErrorBox error={error} /></span>
}

export const text = (data: FormData, name: string) => String(data.get(name) ?? '')
export const number = (data: FormData, name: string) => Number(data.get(name))
