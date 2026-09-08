const DEFAULT_API_BASE_URL = '/api/v1'
const API_BASE_URL = normalizeAPIBaseURL(import.meta.env.VITE_API_BASE_URL)
const ABSOLUTE_URL_PATTERN = /^[a-z][a-z\d+.-]*:\/\//i

function normalizePath(path: string): string {
  return path.startsWith('/') ? path : `/${path}`
}

function normalizeAPIBaseURL(value: unknown): string {
  const raw = String(value || DEFAULT_API_BASE_URL).trim() || DEFAULT_API_BASE_URL
  const withoutTrailingSlash = raw.replace(/\/+$/, '')
  if (/^[a-z][a-z\d+.-]*:\/\//i.test(withoutTrailingSlash) || withoutTrailingSlash.startsWith('//')) {
    return withoutTrailingSlash
  }
  return normalizePath(withoutTrailingSlash)
}

export function getAPIBaseURL(): string {
  return API_BASE_URL
}

function getGatewayBaseURL(): string {
  const apiBaseURL = getAPIBaseURL().replace(/\/+$/, '')
  if (apiBaseURL === DEFAULT_API_BASE_URL) {
    return ''
  }
  if (apiBaseURL.endsWith(DEFAULT_API_BASE_URL)) {
    return apiBaseURL.slice(0, -DEFAULT_API_BASE_URL.length)
  }
  return apiBaseURL
}

export function getGatewayBasePath(): string {
  const baseURL = getGatewayBaseURL().replace(/\/+$/, '')
  if (!baseURL) {
    return ''
  }
  if (ABSOLUTE_URL_PATTERN.test(baseURL) || baseURL.startsWith('//')) {
    const origin = typeof window === 'undefined' ? 'https://localhost' : window.location.origin
    return new URL(baseURL, origin).pathname.replace(/\/+$/, '')
  }
  return normalizePath(baseURL).replace(/\/+$/, '')
}

export function stripGatewayBasePath(path: string): string {
  const normalizedPath = normalizePath(path)
  const basePath = getGatewayBasePath()
  if (!basePath) {
    return normalizedPath
  }
  if (normalizedPath === basePath) {
    return '/'
  }
  if (normalizedPath.startsWith(`${basePath}/`)) {
    return normalizedPath.slice(basePath.length)
  }
  return normalizedPath
}

export function buildApiUrl(path: string): string {
  const base = getAPIBaseURL().replace(/\/+$/, '')
  let suffix = normalizePath(path)
  if (suffix === DEFAULT_API_BASE_URL) {
    suffix = ''
  } else if (suffix.startsWith(`${DEFAULT_API_BASE_URL}/`)) {
    suffix = suffix.slice(DEFAULT_API_BASE_URL.length)
  }
  return `${base}${suffix}`
}

export function buildGatewayUrl(path: string): string {
  const target = `${getGatewayBaseURL().replace(/\/+$/, '')}${normalizePath(path)}`
  if (typeof window === 'undefined') {
    return target
  }
  return new URL(target, window.location.origin).toString()
}
