import { useCallback, useEffect, useRef, useState } from 'react'
import { Button } from '@/components/ui/button'
import { useInstances } from '@/contexts/InstancesContext'
import { deploymentTestsApi } from '@/lib/api'
import { useVisibleRead } from '@/hooks/useVisibleRead'
import type { ControlPatch, ControlSnapshot, DeploymentTest, DeploymentTestRequest, TestEnvelope } from '@/types/deploymentTest'

const storageKey = 'llamactl_deployment_test'
type SavedTest = { instance: string; request: DeploymentTestRequest; id?: string }
function savedTest(): SavedTest | null {
  try {
    const raw = sessionStorage.getItem(storageKey)
    if (!raw) return null
    const value = JSON.parse(raw) as SavedTest
    return value.instance && value.request?.idempotency_key ? value : null
  } catch { return null }
}
const requestID = () => `test-${Array.from(crypto.getRandomValues(new Uint8Array(16)), b => b.toString(16).padStart(2, '0')).join('')}`
const ms = (value: number | null) => value === null ? 'unknown' : `${value.toFixed(1)} ms`

export default function DeploymentTests() {
  const { instances } = useInstances()
  const [saved, setSaved] = useState<SavedTest | null>(savedTest)
  const [name, setName] = useState('')
  const [control, setControl] = useState<ControlSnapshot | null>(null)
  const [model, setModel] = useState('')
  const [context, setContext] = useState('')
  const [layers, setLayers] = useState('')
  const [warmup, setWarmup] = useState(false)
  const [measured, setMeasured] = useState(3)
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [accepted, setAccepted] = useState<DeploymentTest | null>(null)
  const [controlRefresh, setControlRefresh] = useState(0)
  const busy = useRef(false)
  const cancelRequest = useRef<TestEnvelope | null>(null)
  const reportRead = useCallback((signal?: AbortSignal) => {
    if (!saved?.id) return Promise.reject(new Error('尚无已接受的测试 ID'))
    return deploymentTestsApi.get(saved.instance, saved.id, signal)
  }, [saved?.instance, saved?.id])
  const poll = useVisibleRead(reportRead, 1000, !saved?.id)
  const report = poll.data?.id === saved?.id ? poll.data : accepted?.id === saved?.id ? accepted : null

  useEffect(() => {
    setControl(null); setModel('')
    if (!name) return
    const abort = new AbortController()
    void deploymentTestsApi.control(name, abort.signal).then(result => {
      if (!abort.signal.aborted) { setControl(result); setModel(result.actual_model || result.models[0] || '') }
    }).catch(err => { if (!abort.signal.aborted) setError(err instanceof Error ? err.message : '能力读取失败') })
    return () => abort.abort()
  }, [name, controlRefresh, report?.ended_at])

  const remember = (value: SavedTest) => { sessionStorage.setItem(storageKey, JSON.stringify(value)); setSaved(value) }
  const submit = async (retry = false) => {
    if (busy.current) return
    busy.current = true; setPending(true); setError(null)
    let sent = false
    try {
      let submission = retry ? saved : null
      if (!submission) {
        const latest = await deploymentTestsApi.control(name)
        setControl(latest)
        if (latest.recovery_required) throw new Error('需要管理员先核对并恢复 stopped；本面板不会自动恢复或接管进程。')
        if (latest.admission !== 'allowed') throw new Error('ai_paused：owner 已暂停；请由 owner 显式恢复，不自动重试。')
        const patch: ControlPatch = {}
        if (context !== '') patch.context = Number(context)
        if (layers !== '') patch.gpu_layers = Number(layers)
        submission = { instance: name, request: { boot_id: latest.boot_id, expected_revision: latest.revision, idempotency_key: requestID(), model, fixture_id: 'synthetic-text-v1', patch, measured_generations: measured, warmup, max_output_tokens: 256, timeout_seconds: 600, concurrency: 1, seed: 0 } }
        remember(submission)
      }
      sent = true
      const result = await deploymentTestsApi.run(submission.instance, submission.request)
      remember({ ...submission, id: result.id }); setAccepted(result); cancelRequest.current = null
    } catch (err) {
      const message = err instanceof Error ? err.message : '提交失败'
      const rejected = sent && !retry && (/^HTTP (400|403|404|409|413|415|422|429):/.test(message) || message === 'Authentication required')
      if (rejected) { sessionStorage.removeItem(storageKey); setSaved(null); setAccepted(null) }
      setError(rejected ? `${message}。本次新提交被拒绝，可修改输入或刷新能力后明确重新运行；不会自动重试。`
        : `${message}。未自动重放；若响应丢失，可用相同 key 重取原提交。epoch_changed / unknown 不代表从未执行。`)
    }
    finally { busy.current = false; setPending(false) }
  }
  const cancel = async () => {
    if (!saved?.id || busy.current) return
    busy.current = true; setPending(true); setError(null)
    try {
      if (!cancelRequest.current) {
        const latest = await deploymentTestsApi.control(saved.instance)
        cancelRequest.current = { boot_id: saved.request.boot_id, expected_revision: latest.revision, idempotency_key: requestID() }
      }
      setAccepted(await deploymentTestsApi.cancel(saved.instance, saved.id, cancelRequest.current))
      await poll.refresh()
    } catch (err) {
      const message = err instanceof Error ? err.message : '取消失败；请核对当前状态'
      if (message.includes('revision_conflict')) cancelRequest.current = null
      setError(message)
    }
    finally { busy.current = false; setPending(false) }
  }
  const download = async (format: 'json' | 'csv') => {
    if (!saved?.id) return
    try {
      const text = await deploymentTestsApi.export(saved.instance, saved.id, format)
      const url = URL.createObjectURL(new Blob([text], { type: format === 'csv' ? 'text/csv' : 'application/json' }))
      const anchor = document.createElement('a'); anchor.href = url; anchor.download = `deployment-test.${format}`
      anchor.click(); URL.revokeObjectURL(url)
    } catch (err) { setError(err instanceof Error ? err.message : '导出失败') }
  }
  const active = !!saved && (!report || !report.ended_at)
  return <section className="mb-6 rounded border border-border p-4 space-y-3" aria-label="部署测试">
    <h2 className="text-lg font-semibold">单配置部署测试</h2>
    <p className="text-sm text-muted-foreground">固定合成文本 synthetic-text-v1；并发 1、计量 1–3 次、可选 1 次预热，每次最多 256 tokens，总预算 600 秒（含预留清理时间）。可能启动 Router、应用参数并加载模型；默认只卸载本测试加载的模型，保留所测参数，不设为日常默认、不自动恢复旧配置。已有 ready 模型只允许同配置复用且不会卸载。owner 暂停优先。</p>
    <div className="flex flex-wrap gap-3">
      <label>实例 <select aria-label="测试实例" value={name} disabled={pending || active} onChange={e => setName(e.target.value)} className="border rounded bg-background p-1">
        <option value="">选择现有受控 Router</option>
        {(instances || []).map(i => <option key={i.name} value={i.name}>{i.name}</option>)}
      </select></label>
      <label>逻辑模型 <select aria-label="测试模型" value={model} disabled={!control || pending || active} onChange={e => setModel(e.target.value)} className="border rounded bg-background p-1">
        {(control?.models || []).map(id => <option key={id} value={id}>{id}</option>)}
      </select></label>
      <label>Context <input aria-label="测试 context" type="number" min="0" max="1048576" value={context} onChange={e => setContext(e.target.value)} disabled={pending || active} placeholder="保持当前" className="border rounded bg-background p-1 w-28" /></label>
      <label>GPU layers <input aria-label="测试 GPU layers" type="number" min="0" max="999" value={layers} onChange={e => setLayers(e.target.value)} disabled={pending || active} placeholder="保持当前" className="border rounded bg-background p-1 w-28" /></label>
      <label>计量次数 <select aria-label="计量次数" value={measured} disabled={pending || active} onChange={e => setMeasured(Number(e.target.value))} className="border rounded bg-background p-1">{[1, 2, 3].map(n => <option key={n}>{n}</option>)}</select></label>
      <label><input type="checkbox" checked={warmup} disabled={pending || active} onChange={e => setWarmup(e.target.checked)} /> 额外 1 次预热（不计入计量）</label>
    </div>
    {control && <p className="text-sm">{control.state} · revision {control.revision} · {control.admission}{control.recovery_required && ' · 需要管理员显式核对恢复'}</p>}
    <div className="flex flex-wrap gap-2">
      <Button disabled={pending || active || !name || !model || !control || control.recovery_required} onClick={() => void submit()}>运行部署测试</Button>
      {saved && !saved.id && <Button variant="outline" disabled={pending} onClick={() => void submit(true)}>以相同 key 重取本次提交</Button>}
      {saved?.id && <Button variant="outline" disabled={pending || !!report?.ended_at} onClick={() => void cancel()}>取消测试</Button>}
      {saved?.id && <><Button variant="outline" onClick={() => void download('json')}>导出 JSON</Button><Button variant="outline" onClick={() => void download('csv')}>导出 CSV</Button></>}
      <Button variant="outline" disabled={pending || !name} onClick={() => setControlRefresh(n => n + 1)}>刷新控制能力</Button>
      {saved && <Button variant="outline" disabled={pending} onClick={() => {
        if (!report?.ended_at && !window.confirm('清除只影响本地显示，不会取消服务端任务。未知结果可能已执行，不能当作从未执行；请先核对状态再决定是否新建测试。确定清除？')) return
        sessionStorage.removeItem(storageKey); setSaved(null); setAccepted(null); cancelRequest.current = null; setError(null); setControlRefresh(n => n + 1)
      }}>清除本地显示（不取消服务端任务）</Button>}
    </div>
    {(error || poll.error && saved?.id) && <p role="alert" className="text-red-600 break-words">{error || poll.error} 不会自动生成新测试。</p>}
    {report && <div className="space-y-2" aria-live="polite">
      <p><strong>{report.state}</strong> · {report.stage} · 计量完成 {report.completed_measured}/{report.measured_generations} · 已尝试 {report.attempted_generations} 次 · {ms(report.elapsed_ms)}</p>
      {!report.ended_at && <p>202 仅表示接受，尚未成功；关闭网页不会取消任务。</p>}
      {poll.stale && <p>最近读取未经确认 / 已过期；不把旧显示当当前终态。</p>}
      {report.error_code && <p role="alert">{report.error_stage}: {report.error_code}</p>}
      <p>加载：{report.load_mode} · 就绪耗时 {ms(report.ready_duration_ms)} · 清理：{report.cleanup} {report.cleanup_error}</p>
      <p className="text-sm">缓存条件 unknown；请求关闭 prompt cache 不等于已验证冷缓存，initial 不等于 cold。TTFT 取首个非空 content delta，忽略 role / reasoning / usage 帧。未提供的 token / backend timing 为 unknown，不用字数推测。</p>
      <div className="overflow-x-auto"><table className="text-sm w-full"><thead><tr><th>请求</th><th>TTFT</th><th>总时延</th><th>Prompt / completion tokens</th><th>响应 bytes</th><th>结果</th></tr></thead><tbody>{report.samples.map((s, i) => <tr key={i}><td>{s.kind}</td><td>{ms(s.ttft_ms)}</td><td>{ms(s.duration_ms)}</td><td>{s.prompt_tokens ?? 'unknown'} / {s.completion_tokens ?? 'unknown'}</td><td>{s.response_bytes}</td><td>{s.completed ? 'completed' : s.error_code || 'incomplete'}</td></tr>)}</tbody></table></div>
      <p className="text-sm">共享资源缓存：{report.resources.distinct_snapshots} 个不同时间戳快照 · {report.resources.gap || '有观测'} · {report.resources.contention}</p>
      {Object.entries(report.resources.metrics).map(([key, metric]) => <p key={key} className="text-xs">{key}: mean {metric.mean ?? 'unknown'} / observed peak {metric.observed_peak ?? 'unknown'} {metric.unit} · {metric.source} / {metric.scope} · coverage {(metric.coverage * 100).toFixed(0)}% · missing {metric.missing} / stale {metric.stale}</p>)}
      <details><summary>报告身份与有效配置（不含私有路径或文本）</summary><pre className="text-xs overflow-auto">{JSON.stringify({ identity: report.identity, revision: report.revision, process_generation: report.process_generation, load_generation: report.load_generation, parameters: report.parameters, parameter_defaults: report.parameter_defaults, fixture_digest: report.fixture_digest }, null, 2)}</pre></details>
    </div>}
  </section>
}
