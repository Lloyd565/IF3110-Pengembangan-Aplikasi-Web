import { StrictMode, createContext, useContext, useEffect, useState, type ReactNode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter, Link, NavLink, Navigate, Outlet, Route, Routes, useLocation, useOutletContext, useParams } from 'react-router-dom'
import { api, dbPath, setToken } from './api'
import type { Capabilities, Database, User } from './types'
import { Action, Empty, ErrorBox, Field, Form, Loading, Panel, Status, text, useResource } from './ui'
import { Connections, Grants, Groups, Users } from './pages/admin'
import { DataViewer, SchemaDiff, SchemaViewer, TableViewer } from './pages/explorer'
import { MigrationDetail, MigrationEditor, MigrationList } from './pages/migrations'
import { History, JobDetail, Jobs, RequestDetail, Requests } from './pages/workflow'
import './style.css'

type Session = { user: User | null; loading: boolean; capabilities: Capabilities; refresh: () => Promise<void>; signIn: (u: string, p: string) => Promise<void>; signOut: () => Promise<void>; error: unknown }
const SessionContext = createContext<Session | null>(null)
export function useSession() { const s = useContext(SessionContext); if (!s) throw new Error('Session belum tersedia.'); return s }
const unavailable: Capabilities = { workflow: false, jobs: false, leaders: false }

function SessionProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<User | null>(null)
  const [loading, setLoading] = useState(true)
  const [capabilities, setCapabilities] = useState<Capabilities>(unavailable)
  const [error, setError] = useState<unknown>(null)
  async function refresh() {
    try {
      const me = await api<{ user: User; csrf_token: string }>('/api/auth/me')
      setUser(me.user); setToken(me.csrf_token)
      const c = await api<Capabilities>('/api/capabilities'); setCapabilities(c); setError(null)
    } catch (e) {
      if (e instanceof Error && 'status' in e && e.status === 401) { setUser(null); setCapabilities(unavailable); setError(null) } else setError(e)
    } finally { setLoading(false) }
  }
  useEffect(() => {
    void refresh()
    const expire = () => { setUser(null); setCapabilities(unavailable); setToken(null) }
    const permissions = () => { void refresh() }
    window.addEventListener('auth-expired', expire); window.addEventListener('permissions-changed', permissions)
    return () => { window.removeEventListener('auth-expired', expire); window.removeEventListener('permissions-changed', permissions) }
  }, [])
  const value: Session = { user, loading, capabilities, refresh, error,
    signIn: async (username, password) => { const me = await api<{ user: User; csrf_token: string }>('/api/auth/login', { method: 'POST', body: { username, password } }); setToken(me.csrf_token); setUser(me.user); setCapabilities(await api<Capabilities>('/api/capabilities')); setError(null) },
    signOut: async () => { await api('/api/auth/logout', { method: 'POST', body: {} }); setToken(null); setUser(null); setCapabilities(unavailable) },
  }
  return <SessionContext.Provider value={value}>{children}</SessionContext.Provider>
}

function Login() {
  const s = useSession()
  if (s.loading) return <main className="login-screen"><Loading /></main>
  if (s.user) return <Navigate to="/dashboard" replace />
  return <main className="login-screen"><div className="login-brand"><span className="brand-mark">DB</span><p>IF3110 · DATABASE MANAGEMENT</p><h1>Kelola perubahan.<br />Jaga kendali.</h1><p>Satu tempat untuk akses database, peninjauan migration, dan pemantauan pekerjaan.</p></div><section className="login-card"><span className="eyebrow">Selamat datang kembali</span><h2>Masuk ke platform</h2><p className="muted">Gunakan akun platform yang diberikan administrator.</p><ErrorBox error={s.error} retry={() => void s.refresh()} /><Form onSubmit={async d => s.signIn(text(d, 'username'), text(d, 'password'))}><Field label="Username"><input name="username" autoComplete="username" maxLength={200} required autoFocus /></Field><Field label="Password"><input name="password" type="password" autoComplete="current-password" maxLength={1024} required /></Field><button className="full">Masuk</button></Form><p className="muted small">Session dilindungi dengan cookie HttpOnly dan token CSRF.</p></section></main>
}

function Protected() {
  const s = useSession()
  if (s.loading) return <Loading />
  if (s.error && !s.user) return <main><ErrorBox error={s.error} retry={() => void s.refresh()} /></main>
  return s.user ? <Outlet /> : <Navigate to="/login" replace />
}
function Administrator() { return useSession().user?.is_platform_admin ? <Outlet /> : <section><h1>Akses ditolak</h1><p>Halaman ini hanya untuk Platform Admin.</p><Link to="/dashboard">Kembali ke dashboard</Link></section> }

function Layout() {
  const s = useSession(); const [open, setOpen] = useState(false); const location = useLocation()
  useEffect(() => { setOpen(false); window.scrollTo(0, 0) }, [location.pathname])
  return <div className="app-shell"><a className="skip-link" href="#main">Langsung ke konten</a><header className="topbar"><Link className="brand" to="/dashboard"><span className="brand-mark">DB</span><span>IF3110 <small>Database Platform</small></span></Link><div className="header-actions"><span className="username">{s.user?.username}</span><Action run={s.signOut}>Keluar</Action><button className="menu-toggle secondary" type="button" aria-label="Buka navigasi" aria-expanded={open} aria-controls="main-nav" onClick={() => setOpen(!open)}>☰</button></div></header><aside id="main-nav" className={`sidebar ${open ? 'open' : ''}`}><span className="nav-label">WORKSPACE</span><NavLink to="/dashboard">Database saya</NavLink>{s.user?.is_platform_admin && <><span className="nav-label">ADMINISTRASI</span><NavLink to="/admin/users">Pengguna</NavLink><NavLink to="/admin/groups">Group & anggota</NavLink><NavLink to="/admin/connections">Koneksi database</NavLink><NavLink to="/admin/grants">Hak akses</NavLink></>}<div className="sidebar-note">Akses dan setiap perubahan diperiksa kembali oleh server.</div></aside><main id="main" className="workspace"><ErrorBox error={s.error} retry={() => void s.refresh()} /><Outlet /></main></div>
}

function Dashboard() {
  const resource = useResource<Database[]>('/api/databases'); const [query, setQuery] = useState(''); const [group, setGroup] = useState('')
  const groups = [...new Set(resource.data?.flatMap(x => x.groups) ?? [])].sort()
  const list = resource.data?.filter(x => (!group || x.groups.includes(group)) && x.name.toLowerCase().includes(query.toLowerCase()))
  return <><div className="page-head"><div><span className="eyebrow">WORKSPACE</span><h1>Database saya</h1><p className="muted">Jelajahi data dan kelola perubahan sesuai hak akses group.</p></div><span className="count">{resource.data?.length ?? '—'} koneksi</span></div><div className="toolbar"><Field label="Cari database"><input type="search" value={query} onChange={e => setQuery(e.target.value)} placeholder="Nama database" /></Field><Field label="Filter group"><select value={group} onChange={e => setGroup(e.target.value)}><option value="">Semua group</option>{groups.map(x => <option key={x}>{x}</option>)}</select></Field></div><ErrorBox error={resource.error} retry={resource.reload} />{resource.loading && <Loading />}{list?.length === 0 && <Empty>Tidak ada database yang cocok. Hubungi administrator jika Anda belum mendapat grant.</Empty>}<div className="card-grid">{list?.map(db => <article className="db-card" key={db.id}><div className="card-top"><span className="db-icon">{db.dbms === 'mariadb' ? 'M' : 'P'}</span><Status value={db.level} /></div><h2>{db.name}</h2><p className="muted">{db.dbms} · {db.database}</p><p className="small">Group: {db.groups.join(', ')}</p><Link className="button" to={`${dbPath(db.id)}/schema`}>Buka workspace <span aria-hidden>→</span></Link></article>)}</div></>
}

export function useDatabase() { return useOutletContext<Database>() }
function DatabaseLayout() {
  const { id = '' } = useParams(); const resource = useResource<Database[]>('/api/databases'); const db = resource.data?.find(x => x.id === id)
  if (resource.loading) return <Loading />
  if (resource.error) return <ErrorBox error={resource.error} retry={resource.reload} />
  if (!db) return <section><h1>Database tidak tersedia</h1><p>Koneksi tidak ditemukan atau Anda tidak memiliki grant.</p><Link to="/dashboard">Kembali</Link></section>
  return <><div className="page-head"><div><Link className="crumb" to="/dashboard">Database saya /</Link><h1>{db.name}</h1><p className="muted">{db.dbms} · {db.database} · {db.groups.join(', ')}</p></div><Status value={db.level} /></div><nav className="tabs" aria-label="Navigasi database">{[['schema', 'Schema'], ['migrations', 'Migration'], ['requests', 'Review & approval'], ['history', 'History'], ['jobs', 'Job'], ['diff', 'Schema diff']].map(([path, label]) => <NavLink key={path} to={`${dbPath(id)}/${path}`}>{label}</NavLink>)}</nav><Outlet context={db} /></>
}

function NotFound() { return <Panel title="Halaman tidak ditemukan"><Empty>Periksa alamat halaman yang Anda buka.</Empty><Link to="/dashboard">Kembali ke dashboard</Link></Panel> }

function App() {
  return <SessionProvider><Routes><Route path="/login" element={<Login />} /><Route element={<Protected />}><Route element={<Layout />}><Route index element={<Navigate to="/dashboard" replace />} /><Route path="/dashboard" element={<Dashboard />} /><Route element={<Administrator />}><Route path="/admin/users" element={<Users />} /><Route path="/admin/groups" element={<Groups />} /><Route path="/admin/connections" element={<Connections />} /><Route path="/admin/grants" element={<Grants />} /></Route><Route path="/db/:id" element={<DatabaseLayout />}><Route index element={<Navigate to="schema" replace />} /><Route path="schema" element={<SchemaViewer />} /><Route path="tables/:schema/:table" element={<TableViewer />} /><Route path="data/:schema/:table" element={<DataViewer />} /><Route path="diff" element={<SchemaDiff />} /><Route path="schema/diff" element={<SchemaDiff />} /><Route path="migrations" element={<MigrationList />} /><Route path="migrations/new" element={<MigrationEditor />} /><Route path="migrations/:mid" element={<MigrationDetail />} /><Route path="migrations/:mid/edit" element={<MigrationEditor />} /><Route path="history" element={<History />} /><Route path="requests" element={<Requests />} /><Route path="requests/:rid" element={<RequestDetail />} /><Route path="jobs" element={<Jobs />} /><Route path="jobs/:jid" element={<JobDetail />} /></Route><Route path="*" element={<NotFound />} /></Route></Route></Routes></SessionProvider>
}

createRoot(document.getElementById('root')!).render(<StrictMode><BrowserRouter><App /></BrowserRouter></StrictMode>)
