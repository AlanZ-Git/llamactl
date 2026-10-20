import { beforeEach, describe, expect, it, vi } from 'vitest'
import { resourcePolicyApi } from '@/lib/api'

const snapshot = { allow_ai: false, admission: 'blocked', durable: true, release: 'draining', boot_id: 'boot', revision: 4 }

describe('owner policy HTTP contract', () => {
  beforeEach(() => { vi.restoreAllMocks(); sessionStorage.clear() })

  it('uses exact PUT and release bodies and never retries', async () => {
    const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify(snapshot), { status: 200 }))
    global.fetch = fetchMock
    await resourcePolicyApi.update(false, 'boot', 3)
    expect(JSON.parse(fetchMock.mock.calls[0][1].body)).toEqual({ allow_ai: false, boot_id: 'boot', expected_revision: 3 })
    fetchMock.mockResolvedValue(new Response(JSON.stringify(snapshot), { status: 202 }))
    await resourcePolicyApi.release('boot', 4, 'synthetic-request', 'immediate')
    expect(JSON.parse(fetchMock.mock.calls[1][1].body)).toEqual({ boot_id: 'boot', expected_revision: 4, request_id: 'synthetic-request', mode: 'immediate' })
    expect(fetchMock).toHaveBeenCalledTimes(2)
  })

  it('does not log the app out on an owner-only 401 in compatibility mode', async () => {
    const event = vi.fn()
    window.addEventListener('llamactl:unauthorized', event)
    global.fetch = vi.fn().mockResolvedValue(new Response('', { status: 401 }))
    await expect(resourcePolicyApi.get()).rejects.toThrow('Authentication required')
    expect(event).not.toHaveBeenCalled()
    window.removeEventListener('llamactl:unauthorized', event)
  })

  it('rejects malformed or unsafe CAS snapshots', async () => {
    for (const change of [{ revision: 9007199254740992 }, { allow_ai: null }, { admission: 'allowed' }, { boot_id: '' }, { release: 'invented' }]) {
      global.fetch = vi.fn().mockResolvedValue(new Response(JSON.stringify({ ...snapshot, ...change })))
      await expect(resourcePolicyApi.get()).rejects.toThrow()
    }
  })
})
