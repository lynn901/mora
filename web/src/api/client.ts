const BASE_URL = "/api/v1"

interface ApiResponse<T> {
  code: number
  data: T
  message: string
}

export class ApiError extends Error {
  constructor(message: string, public status: number) {
    super(message)
    this.name = "ApiError"
  }
}

const TOKEN_KEY = "mora_token"

export function getToken(): string | null {
  return localStorage.getItem(TOKEN_KEY)
}

export function setToken(token: string): void {
  localStorage.setItem(TOKEN_KEY, token)
}

export function clearToken(): void {
  localStorage.removeItem(TOKEN_KEY)
}

async function request<T>(
  method: string,
  path: string,
  body?: unknown,
  headers?: Record<string, string>,
): Promise<T> {
  const token = getToken()
  const h: Record<string, string> = {
    "Content-Type": "application/json",
    ...headers,
  }
  if (token) h["Authorization"] = `Bearer ${token}`

  const res = await fetch(`${BASE_URL}${path}`, {
    method,
    headers: h,
    body: body ? JSON.stringify(body) : undefined,
  })

  if (res.status === 204) return undefined as T

  const json: ApiResponse<T> = await res.json()

  if (!res.ok || json.code !== 0) {
    const msg = json.message || `Request failed (${res.status})`
    if (res.status === 401) {
      clearToken()
      window.location.reload()
    }
    throw new ApiError(msg, res.status)
  }

  return json.data
}

export const http = {
  get: <T>(path: string, headers?: Record<string, string>) =>
    request<T>("GET", path, undefined, headers),

  post: <T>(path: string, body?: unknown, headers?: Record<string, string>) =>
    request<T>("POST", path, body, headers),

  patch: <T>(path: string, body?: unknown, headers?: Record<string, string>) =>
    request<T>("PATCH", path, body, headers),

  delete: <T>(path: string, headers?: Record<string, string>) =>
    request<T>("DELETE", path, undefined, headers),
}

export async function login(email: string, password: string): Promise<{ token: string; user: { id: string; email: string; name: string } }> {
  const data = await http.post<{ token: string; user: { id: string; email: string; name: string } }>("/auth/login", { email, password })
  setToken(data.token)
  return data
}

export function authHeaders(): Record<string, string> {
  const token = getToken()
  return token ? { Authorization: `Bearer ${token}` } : {}
}
