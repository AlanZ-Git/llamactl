import { beforeEach, describe, expect, it, vi } from 'vitest'
import { deploymentTestsApi } from '@/lib/api'

describe('deployment HTTP client contract', () => {
  beforeEach(() => { vi.restoreAllMocks(); sessionStorage.clear() })
  it('uses fixed resources, unchanged JSON presence and authenticated export without a credential URL', async () => {
    sessionStorage.setItem('llamactl_management_key', 'synthetic-management-key')
    const fetchMock = vi.fn().mockImplementation(() => Promise.resolve(new Response('{}', { status: 200 })))
    global.fetch = fetchMock
    const body = { boot_id: 'boot', expected_revision: 0, idempotency_key: 'request', model: 'alpha', fixture_id: 'synthetic-text-v1' as const, patch: { context: 0, threads: null, flash_attention: false } }
    await deploymentTestsApi.run('router', body)
    await deploymentTestsApi.get('router', 'run')
    await deploymentTestsApi.cancel('router', 'run', { boot_id: 'boot', expected_revision: 1, idempotency_key: 'cancel' })
    await deploymentTestsApi.export('router', 'run', 'csv')
    expect(fetchMock).toHaveBeenCalledTimes(4)
    expect(JSON.parse(fetchMock.mock.calls[0][1].body)).toEqual(body)
    expect(fetchMock.mock.calls[0][0]).toMatch(/\/instances\/router\/tests$/)
    expect(fetchMock.mock.calls[1][0]).toMatch(/\/instances\/router\/tests\/run$/)
    expect(fetchMock.mock.calls[2][0]).toMatch(/\/instances\/router\/tests\/run\/cancel$/)
    expect(fetchMock.mock.calls[3][0]).toMatch(/\/instances\/router\/tests\/run\?format=csv$/)
    for (const [url, options] of fetchMock.mock.calls) {
      expect(url).not.toContain('synthetic-management-key')
      expect(options.headers.Authorization).toBe('Bearer synthetic-management-key')
    }
  })
  it('does not retry epoch/unknown/rejection responses', async () => {
    for (const [status, error] of [[409, 'epoch_changed'], [404, 'test_unknown_not_resumable'], [422, 'invalid_test_budget']] as const) {
      const fetchMock = vi.fn().mockResolvedValue(new Response(JSON.stringify({ error }), { status }))
      global.fetch = fetchMock
      await expect(deploymentTestsApi.get('router', 'old')).rejects.toThrow(error)
      expect(fetchMock).toHaveBeenCalledTimes(1)
    }
  })
})
