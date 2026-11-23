// Thin client for the factory API. A 401 asks the app to show the sign-in dialog.

export class ApiError extends Error {
  status: number
  constructor(status: number, message: string) {
    super(message)
    this.status = status
  }
}

type Listener = () => void
const unauthorized = new Set<Listener>()
export const onUnauthorized = (fn: Listener) => {
  unauthorized.add(fn)
  return () => {
    unauthorized.delete(fn)
  }
}

export async function api(path: string, init?: RequestInit): Promise<Response> {
  const r = await fetch(path, init)
  if (r.status === 401) unauthorized.forEach((fn) => fn())
  if (!r.ok) {
    let msg = r.statusText
    try {
      msg = (await r.json()).error || msg
    } catch {
      /* not JSON */
    }
    throw new ApiError(r.status, msg)
  }
  return r
}

export const getJSON = async <T,>(path: string): Promise<T> => (await api(path)).json() as Promise<T>

export const postJSON = async <T,>(path: string, body: unknown): Promise<T> =>
  (await api(path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) })).json() as Promise<T>
