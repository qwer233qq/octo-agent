import { describe, it, expect, beforeEach } from 'vitest'
import { authHeaders, storedKey, wsQuery } from './auth'

// The header/query fallbacks back the Secure-cookie wedge: an https visit
// stores `octo_access_key` with the Secure attribute, and on an http page
// that cookie is neither readable, send-able, nor overwritable — so the
// stored key must ride as X-Access-Key on fetches and ?access_key= on the
// WebSocket handshake instead.

beforeEach(() => {
  localStorage.clear()
})

describe('storedKey', () => {
  it('reads the last accepted key from localStorage', () => {
    expect(storedKey()).toBeNull()
    localStorage.setItem('octo_access_key', 'zzzzzz')
    expect(storedKey()).toBe('zzzzzz')
  })
})

describe('authHeaders', () => {
  it('adds the raw key as X-Access-Key only when one is stored', () => {
    expect(authHeaders()).toEqual({})
    localStorage.setItem('octo_access_key', 'zz%zz+zz')
    // Raw, not encoded: the server reads the header verbatim while the
    // cookie value goes through PathUnescape — encoding here would corrupt
    // keys containing % or +.
    expect(authHeaders()).toEqual({ 'X-Access-Key': 'zz%zz+zz' })
  })
})

describe('wsQuery', () => {
  it('returns the path untouched when no key is stored', () => {
    expect(wsQuery('/ws')).toBe('/ws')
  })

  it('appends the key as a query parameter, URL-encoded', () => {
    localStorage.setItem('octo_access_key', 'a+b%2Fc')
    expect(wsQuery('/ws')).toBe('/ws?access_key=a%2Bb%252Fc')
  })
})
