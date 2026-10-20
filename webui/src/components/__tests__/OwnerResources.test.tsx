import { act, fireEvent, render, renderHook, screen, waitFor } from '@testing-library/react'
import { useVisibleRead } from '@/hooks/useVisibleRead'
import { beforeEach, afterEach, describe, expect, it, vi } from 'vitest'
import OwnerResources from '@/components/OwnerResources'
import { resourcePolicyApi, resourcesApi } from '@/lib/api'
import type { ResourceMetric, ResourceSnapshot } from '@/types/resources'

vi.mock('@/lib/api', () => ({
  resourcePolicyApi: { get: vi.fn(), update: vi.fn(), release: vi.fn() },
  resourcesApi: { get: vi.fn() },
}))
const allowed = { allow_ai: true, admission: 'allowed' as const, durable: true, release: 'not_requested' as const, boot_id: 'boot', revision: 1 }
const blocked = { ...allowed, allow_ai: false, admission: 'blocked' as const, release: 'draining' as const, revision: 2 }
const deferred = <T,>() => { let resolve!: (v: T) => void; let reject!: (e: Error) => void; const promise = new Promise<T>((a, b) => { resolve = a; reject = b }); return { promise, resolve, reject } }

beforeEach(() => {
  vi.clearAllMocks()
  Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' })
  vi.mocked(resourcePolicyApi.get).mockResolvedValue(allowed)
  vi.mocked(resourcesApi.get).mockRejectedValue(new Error('metrics unavailable'))
})
afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks() })

describe('owner controls', () => {
  it('shows source timestamps, cadence, null reasons, stale metrics and non-attribution', async () => {
    const metric: ResourceMetric = { value: null, unit: 'bytes', source: 'fixture_native', scope: 'physical_memory', available: false, reason: 'synthetic_unavailable', sampled_at: null, sample_window_ms: 0, stale: true }
    const data: ResourceSnapshot = {
      sampled_at: '2020-01-01T00:00:00Z', sample_window_ms: 5, interval_ms: 10000, target_interval_ms: 10000,
      stale: true, throttled: true, throttle_reason: 'owner_paused_or_idle', gaming: metric,
      cpu: { usage: metric, logical_count: metric, physical_count: metric },
      memory: { total: metric, available: metric, used: metric },
      gpu: { source: 'nvidia-smi', scope: 'whole_device', available: false, reason: 'nvidia_smi_timeout', sampled_at: null, attempted_at: '2020-01-01T00:00:00Z', sample_window_ms: 0, stale: true, devices: [] },
      processes: [], process_coverage: 'manager_owned_local_only',
    }
    vi.mocked(resourcesApi.get).mockResolvedValue(data)
    render(<OwnerResources />)
    await screen.findByText(/实际间隔 10000 ms/)
    expect(screen.getByText(/数据过期，请勿作为当前值/)).toBeInTheDocument()
    expect(screen.getByText(/GPU: nvidia_smi_timeout/)).toBeInTheDocument()
    expect(screen.getAllByText('synthetic_unavailable').length).toBe(6)
    expect(screen.getByText(/包含未受管理的外部程序占用/)).toBeInTheDocument()
  })

  it('pauses with exact CAS; pending prevents duplicate clicks; only GET verifies state', async () => {
    const write = deferred<typeof blocked>()
    vi.mocked(resourcePolicyApi.update).mockReturnValue(write.promise)
    render(<OwnerResources />)
    const button = await screen.findByRole('button', { name: '暂停 AI' })
    await waitFor(() => expect(button).toBeEnabled())
    fireEvent.click(button); fireEvent.click(button)
    expect(resourcePolicyApi.update).toHaveBeenCalledExactlyOnceWith(false, 'boot', 1, expect.any(AbortSignal))
    expect(button).toBeDisabled()
    vi.mocked(resourcePolicyApi.get).mockResolvedValue(blocked)
    await act(async () => write.resolve(blocked))
    await waitFor(() => expect(screen.getByRole('button', { name: '恢复 AI' })).toBeEnabled())
    expect(resourcePolicyApi.get).toHaveBeenCalledTimes(2)
    expect(resourcePolicyApi.release).not.toHaveBeenCalled()
    expect(screen.getByText(/排空中/)).toBeInTheDocument()
  })

  it('resumes admission without any start/load/replay', async () => {
    vi.mocked(resourcePolicyApi.get).mockResolvedValue(blocked)
    vi.mocked(resourcePolicyApi.update).mockResolvedValue({ ...allowed, revision: 3 })
    render(<OwnerResources />)
    const button = await screen.findByRole('button', { name: '恢复 AI' })
    await waitFor(() => expect(button).toBeEnabled())
    vi.mocked(resourcePolicyApi.get).mockResolvedValue({ ...allowed, revision: 3 })
    fireEvent.click(button)
    await waitFor(() => expect(screen.getByRole('button', { name: '暂停 AI' })).toBeEnabled())
    expect(resourcePolicyApi.update).toHaveBeenCalledExactlyOnceWith(true, 'boot', 2, expect.any(AbortSignal))
    expect(screen.getByText(/不会自动加载或重放/)).toBeInTheDocument()
  })

  it('requires separate immediate confirmation, supports cancel, and never treats 202 as released', async () => {
    vi.mocked(resourcePolicyApi.get).mockResolvedValue(blocked)
    vi.mocked(resourcePolicyApi.release).mockResolvedValue({ ...blocked, release: 'terminating' })
    render(<OwnerResources />)
    const button = await screen.findByRole('button', { name: '立即释放…' })
    await waitFor(() => expect(button).toBeEnabled())
    fireEvent.click(button)
    expect(resourcePolicyApi.release).not.toHaveBeenCalled()
    fireEvent.click(screen.getByRole('button', { name: '取消' }))
    expect(resourcePolicyApi.release).not.toHaveBeenCalled()
    fireEvent.click(button)
    vi.mocked(resourcePolicyApi.get).mockResolvedValue({ ...blocked, release: 'terminating' })
    fireEvent.click(screen.getByRole('button', { name: '确认立即释放' }))
    await waitFor(() => expect(resourcePolicyApi.release).toHaveBeenCalledOnce())
    expect(resourcePolicyApi.release).toHaveBeenCalledWith('boot', 2, expect.stringMatching(/^[\w.-]{1,128}$/), 'immediate', expect.any(AbortSignal))
    await screen.findByText(/终止中/)
    expect(screen.queryByText(/^已释放$/)).not.toBeInTheDocument()
  })

  it.each(['HTTP 409: conflict', 'network unknown'])('reconciles %s before another intent and keeps the error', async (message) => {
    const readback = deferred<typeof blocked>()
    vi.mocked(resourcePolicyApi.update).mockRejectedValue(new Error(message))
    render(<OwnerResources />)
    const button = await screen.findByRole('button', { name: '暂停 AI' })
    await waitFor(() => expect(button).toBeEnabled())
    vi.mocked(resourcePolicyApi.get).mockReturnValue(readback.promise)
    fireEvent.click(button)
    await waitFor(() => expect(resourcePolicyApi.get).toHaveBeenCalledTimes(2))
    expect(button).toBeDisabled()
    fireEvent.click(button)
    expect(resourcePolicyApi.update).toHaveBeenCalledOnce()
    await act(async () => readback.resolve(blocked))
    await waitFor(() => expect(screen.getByRole('button', { name: '恢复 AI' })).toBeEnabled())
    expect(screen.getByText(new RegExp(message))).toBeInTheDocument()
    expect(screen.getByText(/未自动重试/)).toBeInTheDocument()
  })

  it('failed readback keeps writes blocked even after a successful write response', async () => {
    vi.mocked(resourcePolicyApi.update).mockResolvedValue(blocked)
    render(<OwnerResources />)
    const button = await screen.findByRole('button', { name: '暂停 AI' })
    await waitFor(() => expect(button).toBeEnabled())
    vi.mocked(resourcePolicyApi.get).mockRejectedValue(new Error('offline'))
    fireEvent.click(button)
    await screen.findByText(/offline/)
    expect(screen.getByRole('button', { name: '暂停 AI' })).toBeDisabled()
  })

  it('owner unauthorized stays visible with no automatic retry loop', async () => {
    vi.mocked(resourcePolicyApi.get).mockRejectedValue(new Error('Authentication required'))
    render(<OwnerResources />)
    await screen.findByText(/Authentication required/)
    expect(screen.getByRole('button', { name: '暂停 AI' })).toBeDisabled()
    expect(screen.getByText(/owner 会话或显式配置的管理密钥/)).toBeInTheDocument()
  })

  it('clears panel timers after zero-delay framework teardown, without waiting for request timeouts', async () => {
    vi.useFakeTimers()
    vi.mocked(resourcePolicyApi.get).mockReturnValue(new Promise(() => {}))
    vi.mocked(resourcesApi.get).mockReturnValue(new Promise(() => {}))
    const view = render(<OwnerResources />)
    view.unmount()
    await act(async () => vi.advanceTimersByTimeAsync(0))
    expect(vi.getTimerCount()).toBe(0)
  })

  it('clears all hook polling and request timeout timers immediately on unmount', async () => {
    vi.useFakeTimers()
    vi.mocked(resourcePolicyApi.get).mockReturnValue(new Promise(() => {}))
    const view = renderHook(() => useVisibleRead(resourcePolicyApi.get, 2000))
    expect(vi.getTimerCount()).toBeGreaterThan(0)
    view.unmount()
    expect(vi.getTimerCount()).toBe(0)
  })

  it('aborts outstanding reads on unmount and ignores late snapshots', async () => {
    const read = deferred<typeof allowed>()
    vi.mocked(resourcePolicyApi.get).mockReturnValue(read.promise)
    const view = render(<OwnerResources />)
    const signal = vi.mocked(resourcePolicyApi.get).mock.calls[0][0]
    view.unmount()
    expect(signal?.aborted).toBe(true)
    await act(async () => read.resolve(allowed))
    expect(resourcePolicyApi.update).not.toHaveBeenCalled()
  })

  it('checks freshness again at click time, not only when rendering the button', async () => {
    render(<OwnerResources />)
    const button = await screen.findByRole('button', { name: '暂停 AI' })
    await waitFor(() => expect(button).toBeEnabled())
    const oldNow = Date.now()
    vi.spyOn(Date, 'now').mockReturnValue(oldNow + 20000)
    fireEvent.click(button)
    expect(resourcePolicyApi.update).not.toHaveBeenCalled()
    expect(screen.getByText(/未发送写入/)).toBeInTheDocument()
  })

  it('invalidates confirmation when a newer policy is read', async () => {
    vi.mocked(resourcePolicyApi.get).mockResolvedValue(blocked)
    render(<OwnerResources />)
    const release = await screen.findByRole('button', { name: '立即释放…' })
    await waitFor(() => expect(release).toBeEnabled())
    fireEvent.click(release)
    vi.mocked(resourcePolicyApi.get).mockResolvedValue({ ...blocked, revision: 8 })
    act(() => document.dispatchEvent(new Event('visibilitychange')))
    await waitFor(() => expect(screen.getByRole('button', { name: '确认立即释放' })).toBeEnabled())
    fireEvent.click(screen.getByRole('button', { name: '确认立即释放' }))
    expect(resourcePolicyApi.release).not.toHaveBeenCalled()
    expect(screen.getByText(/确认期间策略已改变/)).toBeInTheDocument()
  })

  it('does not poll hidden; visibility invalidates stale policy and unmount aborts reads', async () => {
    vi.useFakeTimers()
    await act(async () => { render(<OwnerResources />) })
    expect(screen.getByRole('button', { name: '暂停 AI' })).toBeEnabled()
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' })
    act(() => document.dispatchEvent(new Event('visibilitychange')))
    const calls = vi.mocked(resourcePolicyApi.get).mock.calls.length
    await act(async () => vi.advanceTimersByTimeAsync(20000))
    expect(resourcePolicyApi.get).toHaveBeenCalledTimes(calls)
    expect(screen.getByRole('button', { name: '暂停 AI' })).toBeDisabled()
    Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' })
    vi.mocked(resourcePolicyApi.get).mockReturnValue(new Promise(() => {}))
    act(() => document.dispatchEvent(new Event('visibilitychange')))
    expect(screen.getByRole('button', { name: '暂停 AI' })).toBeDisabled()
  })
})
