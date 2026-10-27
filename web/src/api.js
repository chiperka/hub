import { reactive } from 'vue'

export const session = reactive({ loaded: false, authenticated: false, authDisabled: false, setupRequired: false, principal: null, version: '' })

export class ApiError extends Error {
  constructor(status, message) {
    super(message)
    this.status = status
  }
}

async function request(method, url, body) {
  const opts = { method, headers: {}, credentials: 'same-origin' }
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json'
    opts.body = JSON.stringify(body)
  }
  const res = await fetch(url, opts)
  if (res.status === 401) {
    session.authenticated = false
    if (!location.pathname.startsWith('/login')) location.href = '/login?next=' + encodeURIComponent(location.pathname + location.search)
    throw new ApiError(401, 'authentication required')
  }
  if (res.status === 204) return null
  const ct = res.headers.get('content-type') || ''
  const data = ct.includes('application/json') ? await res.json() : await res.text()
  if (!res.ok) throw new ApiError(res.status, (data && data.error) || res.statusText)
  return data
}

export const api = {
  get: (url) => request('GET', url),
  post: (url, body) => request('POST', url, body ?? {}),
  patch: (url, body) => request('PATCH', url, body),
  del: (url) => request('DELETE', url),
}

export async function loadSession() {
  const s = await fetch('/api/auth/state', { credentials: 'same-origin' }).then((r) => r.json())
  session.loaded = true
  session.authenticated = s.authenticated
  session.authDisabled = s.auth_disabled
  session.setupRequired = s.setup_required
  session.principal = s.principal
  session.version = s.version
  return session
}
