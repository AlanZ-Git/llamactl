import { act, fireEvent, render, screen, waitFor } from '@testing-library/react'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import DeploymentTests from '@/components/DeploymentTests'
import { deploymentTestsApi } from '@/lib/api'
import type { ControlSnapshot, DeploymentTest } from '@/types/deploymentTest'

vi.mock('@/contexts/InstancesContext', () => ({ useInstances: () => ({ instances: [{ id: 1, name: 'router', status: 'stopped' }] }) }))
vi.mock('@/lib/api', () => ({ deploymentTestsApi: { control: vi.fn(), run: vi.fn(), get: vi.fn(), cancel: vi.fn(), export: vi.fn() } }))
const control: ControlSnapshot = {
  protocol_version: 'p3a/1', actions: [], operation_budget_seconds: 600, operation_capacity: 1024, idempotency_retention_seconds: 86400,
  admission: 'allowed', boot_id: 'boot', revision: 0, state: 'stopped', running: false, process_generation: 0, load_generation: 0,
  models: ['alpha'], parameters: {}, configured_parameters: { alpha: {} }, ready_probe: 'synthetic', recovery_required: false,
}
const running: DeploymentTest = {
  id: 'run', boot_id: 'boot', instance: 'router', model: 'alpha', state: 'running', stage: 'accepted', started_at: '2026-01-01T00:00:00Z', deadline: '2026-01-01T00:10:00Z', ended_at: null, elapsed_ms: 0,
  revision: 1, process_generation: 1, load_generation: 1, parameters: {}, parameter_defaults: 'unknown engine defaults',
  identity: { application_version: 'synthetic', application_commit: 'unknown', application_build_time: 'unknown', source_modified: null, engine: 'llama_cpp', engine_version: 'unknown', engine_hash: 'unknown', model_hash: 'unknown', runtime_identity: 'unknown' },
  fixture_id: 'synthetic-text-v1', fixture_digest: '0'.repeat(64), measured_generations: 3, warmup: false, max_output_tokens: 256, seed: 0, temperature: 0, concurrency: 1, attempted_generations: 0, completed_measured: 0,
  load_mode: 'test_owned', cache_condition: 'unknown', cache_prompt_requested: false, ready_duration_ms: null, cleanup: 'pending', cleanup_duration_ms: 0, samples: [],
  resources: { source: 'shared_resource_cache', baseline_at: null, baseline: {}, metrics: {}, distinct_snapshots: 0, gap: 'baseline_unavailable', contention: 'unknown' },
}
const done: DeploymentTest = { ...running, state: 'succeeded', ended_at: '2026-01-01T00:00:02Z', stage: 'finished', completed_measured: 3, attempted_generations: 3, cleanup: 'unloaded_test_owned' }
const stored = { instance: 'router', request: { boot_id: 'boot', expected_revision: 0, idempotency_key: 'key', model: 'alpha', fixture_id: 'synthetic-text-v1' }, id: 'old-run' }
const selectRouter = async () => {
  fireEvent.change(screen.getByLabelText('测试实例'), { target: { value: 'router' } })
  await waitFor(() => expect(screen.getByRole('button', { name: '运行部署测试' })).toBeEnabled())
}
beforeEach(() => {
  vi.resetAllMocks(); sessionStorage.clear()
  Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'visible' })
  vi.mocked(deploymentTestsApi.control).mockResolvedValue(control)
  vi.mocked(deploymentTestsApi.run).mockResolvedValue(running)
  vi.mocked(deploymentTestsApi.get).mockResolvedValue(running)
  vi.mocked(deploymentTestsApi.cancel).mockResolvedValue({ ...running, state: 'cancelled', ended_at: done.ended_at })
})
afterEach(() => { vi.restoreAllMocks() })

describe('single deployment test panel', () => {
  it('runs bounded defaults once, shows accepted not success, cancels and exports via authenticated API', async () => {
    const create = vi.fn(() => 'blob:synthetic')
    vi.stubGlobal('URL', Object.assign(URL, { createObjectURL: create, revokeObjectURL: vi.fn() }))
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
    vi.mocked(deploymentTestsApi.export).mockResolvedValue('field,value\nid,run\n')
    render(<DeploymentTests />)
    await selectRouter()
    fireEvent.change(screen.getByLabelText('测试 GPU layers'), { target: { value: '0' } })
    fireEvent.click(screen.getByRole('button', { name: '运行部署测试' }))
    fireEvent.click(screen.getByRole('button', { name: '运行部署测试' }))
    await screen.findByText(/202 仅表示接受/)
    expect(deploymentTestsApi.run).toHaveBeenCalledTimes(1)
    expect(deploymentTestsApi.run).toHaveBeenCalledWith('router', expect.objectContaining({ boot_id: 'boot', expected_revision: 0, patch: { gpu_layers: 0 }, measured_generations: 3, warmup: false, max_output_tokens: 256, concurrency: 1, timeout_seconds: 600, fixture_id: 'synthetic-text-v1' }))
    vi.mocked(deploymentTestsApi.control).mockResolvedValue({ ...control, revision: 1 })
    fireEvent.click(screen.getByRole('button', { name: '取消测试' }))
    await waitFor(() => expect(deploymentTestsApi.cancel).toHaveBeenCalledWith('router', 'run', expect.objectContaining({ boot_id: 'boot', expected_revision: 1 })))
    fireEvent.click(screen.getByRole('button', { name: '导出 CSV' }))
    await waitFor(() => expect(deploymentTestsApi.export).toHaveBeenCalledWith('router', 'run', 'csv'))
    expect(create).toHaveBeenCalled()
    expect(deploymentTestsApi.run).toHaveBeenCalledTimes(1)
  })

  it.each(['HTTP 422: invalid_test_budget', 'HTTP 409: ready_configuration_conflict', 'HTTP 409: ai_paused'])('definite rejection %s unlocks editing without replay', async message => {
    vi.mocked(deploymentTestsApi.run).mockRejectedValue(new Error(message))
    render(<DeploymentTests />); await selectRouter()
    fireEvent.click(screen.getByRole('button', { name: '运行部署测试' }))
    await screen.findByText(new RegExp(message))
    expect(screen.getByLabelText('测试 context')).toBeEnabled()
    expect(screen.getByRole('button', { name: '运行部署测试' })).toBeEnabled()
    expect(deploymentTestsApi.run).toHaveBeenCalledTimes(1)
    expect(sessionStorage.getItem('llamactl_deployment_test')).toBeNull()
  })

  it('preserves uncertain submission and only explicitly retries identical payload/key', async () => {
    vi.mocked(deploymentTestsApi.run).mockRejectedValueOnce(new Error('network unknown'))
    const first = render(<DeploymentTests />); await selectRouter()
    fireEvent.click(screen.getByRole('button', { name: '运行部署测试' }))
    await screen.findByText(/network unknown/)
    const original = vi.mocked(deploymentTestsApi.run).mock.calls[0]
    first.unmount()
    render(<DeploymentTests />)
    expect(deploymentTestsApi.run).toHaveBeenCalledTimes(1)
    fireEvent.click(screen.getByRole('button', { name: '以相同 key 重取本次提交' }))
    await screen.findByText(/202 仅表示接受/)
    expect(vi.mocked(deploymentTestsApi.run).mock.calls[1]).toEqual(original)
  })

  it('restart unknown can be explicitly forgotten but never replayed automatically', async () => {
    sessionStorage.setItem('llamactl_deployment_test', JSON.stringify(stored))
    vi.mocked(deploymentTestsApi.get).mockRejectedValue(new Error('HTTP 404: test_unknown_not_resumable'))
    vi.spyOn(window, 'confirm').mockReturnValue(true)
    render(<DeploymentTests />)
    await screen.findByText(/test_unknown_not_resumable/)
    const clear = screen.getByRole('button', { name: '清除本地显示（不取消服务端任务）' })
    expect(clear).toBeEnabled()
    fireEvent.click(clear)
    expect(window.confirm).toHaveBeenCalled()
    expect(sessionStorage.getItem('llamactl_deployment_test')).toBeNull()
    expect(deploymentTestsApi.run).not.toHaveBeenCalled()
    expect(deploymentTestsApi.cancel).not.toHaveBeenCalled()
    expect(screen.getByLabelText('测试实例')).toBeEnabled()
  })

  it('refreshes capability after terminal and shows resource/cache gaps without fake metrics', async () => {
    vi.mocked(deploymentTestsApi.get).mockResolvedValue(done)
    render(<DeploymentTests />); await selectRouter()
    fireEvent.click(screen.getByRole('button', { name: '运行部署测试' }))
    await screen.findByText('succeeded')
    await waitFor(() => expect(vi.mocked(deploymentTestsApi.control).mock.calls.length).toBeGreaterThanOrEqual(3))
    expect(screen.getByText(/baseline_unavailable/)).toBeInTheDocument()
    expect(screen.getByText(/缓存条件 unknown/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: '运行部署测试' })).toBeEnabled()
    expect(deploymentTestsApi.run).toHaveBeenCalledTimes(1)
  })

  it('paused owner blocks a new write and hidden/unmounted panels do not replay', async () => {
    const view = render(<DeploymentTests />); await selectRouter()
    vi.mocked(deploymentTestsApi.control).mockResolvedValue({ ...control, admission: 'blocked' })
    fireEvent.click(screen.getByRole('button', { name: '运行部署测试' }))
    await screen.findByText(/ai_paused/)
    await act(async () => { Object.defineProperty(document, 'visibilityState', { configurable: true, value: 'hidden' }); document.dispatchEvent(new Event('visibilitychange')) })
    view.unmount()
    expect(deploymentTestsApi.run).not.toHaveBeenCalled()
  })
})
