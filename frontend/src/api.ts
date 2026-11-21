let token: string | null = null

export class ApiError extends Error {
  constructor(public status: number, public code: string, message: string) { super(message) }
}

export function setToken(value: string | null) { token = value }

function endpoint(path: string) {
  const url = new URL(path, window.location.origin)
  if (url.origin !== window.location.origin || !url.pathname.startsWith('/api/') || !path.startsWith('/api/')) {
    throw new Error('Alamat API tidak diizinkan.')
  }
  return url
}

export async function api<T>(path: string, options: {
  method?: string; body?: unknown; signal?: AbortSignal; key?: string; timeout?: number
} = {}): Promise<T> {
  const url = endpoint(path)
  const method = options.method ?? 'GET'
  const headers: Record<string, string> = { Accept: 'application/json' }
  const timeout = AbortSignal.timeout(options.timeout ?? 20000)
  const signal = options.signal ? AbortSignal.any([options.signal, timeout]) : timeout
  if (!['GET', 'HEAD', 'OPTIONS'].includes(method)) {
    if (!token) {
      let response: Response
      try { response = await fetch(endpoint('/api/auth/csrf'), { credentials: 'same-origin', headers: { Accept: 'application/json' }, signal }) }
      catch (error) { if (options.signal?.aborted) throw error; throw new ApiError(0, 'network', 'Token keamanan belum dapat dimuat. Periksa koneksi lalu coba lagi.') }
      const payload = await response.json()
      if (!response.ok || !payload.data?.csrf_token) throw new ApiError(response.status, 'csrf_unavailable', 'Token keamanan belum dapat dimuat. Muat ulang halaman.')
      token = payload.data.csrf_token
    }
    headers['X-CSRF-Token'] = token!
  }
  if (options.body !== undefined) headers['Content-Type'] = 'application/json'
  if (options.key) headers['Idempotency-Key'] = options.key
  let response: Response
  try {
    response = await fetch(url, { method, credentials: 'same-origin', headers, signal, body: options.body === undefined ? undefined : JSON.stringify(options.body) })
  } catch (error) {
    if (options.signal?.aborted) throw error
    throw new ApiError(0, 'network', 'Koneksi terputus atau permintaan melewati batas waktu. Periksa koneksi lalu coba lagi.')
  }
  const payload = await response.json().catch(() => null)
  if (!response.ok) {
    if (response.status === 401) {
      token = null
      window.dispatchEvent(new Event('auth-expired'))
    }
    if (response.status === 403) {
      token = null
      window.dispatchEvent(new Event('permissions-changed'))
    }
    throw new ApiError(response.status, payload?.error?.code ?? 'http', payload?.error?.message ?? `Permintaan gagal (${response.status}).`)
  }
  if (!payload || !Object.prototype.hasOwnProperty.call(payload, 'data')) throw new ApiError(502, 'invalid_response', 'Respons layanan tidak sesuai format API.')
  return payload.data as T
}

export const enc = encodeURIComponent
export const dbApi = (id: string) => `/api/db/${enc(id)}`
export const dbPath = (id: string) => `/db/${enc(id)}`
export function newKey() { return Array.from(crypto.getRandomValues(new Uint8Array(16)), b => b.toString(16).padStart(2, '0')).join('') }
