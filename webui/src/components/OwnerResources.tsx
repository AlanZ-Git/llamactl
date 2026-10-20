import { useEffect, useRef, useState } from 'react'
import { Button } from '@/components/ui/button'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { resourcePolicyApi, resourcesApi } from '@/lib/api'
import { useVisibleRead } from '@/hooks/useVisibleRead'
import type { ResourceMetric, ResourcePolicy, ResourceSnapshot } from '@/types/resources'

const releaseCopy: Record<ResourcePolicy['release'], string> = {
  not_requested: '未请求释放', draining: '排空中：等待已准入请求完成', terminating: '终止中：等待进程退出证据',
  released: '已释放：管理器已确认所持进程树退出与端口证据',
  unreleased_unknown: '释放未知：尚无完整退出证据', failed: '释放失败：请查看错误并重新核对',
}
const timestamp = (value: string | null) => value ? new Date(value).toLocaleString() : '尚无有效采样'
const releaseRequestID = () => `owner-${Array.from(crypto.getRandomValues(new Uint8Array(16)), byte => byte.toString(16).padStart(2, '0')).join('')}`

function Metric({ label, metric, stale }: { label: string; metric: ResourceMetric; stale: boolean }) {
  const known = metric.available && metric.value !== null && Number.isFinite(metric.value)
  const value = known ? metric.unit === 'bytes'
    ? `${(metric.value! / 1024 ** 3).toFixed(2)} GiB`
    : `${metric.value!.toLocaleString(undefined, { maximumFractionDigits: 1 })}${metric.unit.startsWith('percent') ? '%' : ''}`
    : '—'
  return <div className="min-w-0 rounded border border-border p-2">
    <div className="text-sm">{label}: <strong>{value}</strong>{(stale || metric.stale) && <span className="ml-2 text-amber-600">过期 / 不可用</span>}</div>
    {!known && <p className="text-xs text-muted-foreground break-words">{metric.reason || '未提供数值'}</p>}
    <p className="text-xs text-muted-foreground break-words">{metric.source} · {metric.scope} · {metric.unit}</p>
    <p className="text-xs text-muted-foreground">{timestamp(metric.sampled_at)} · 窗口 {metric.sample_window_ms} ms</p>
  </div>
}

function ResourceValues({ data, stale }: { data: ResourceSnapshot; stale: boolean }) {
  const age = data.sampled_at ? Date.now() - Date.parse(data.sampled_at) : Infinity
  stale = stale || data.stale || age < 0 || age > Math.max(5000, data.target_interval_ms * 2)
  return <div className="space-y-3">
    <p className="text-xs text-muted-foreground">
      采样 {timestamp(data.sampled_at)} · 本轮耗时 {data.sample_window_ms} ms · 实际间隔 {data.interval_ms} ms · 目标间隔 {data.target_interval_ms} ms
      {data.throttled ? ` · 已降频 (${data.throttle_reason})` : ' · 活跃采样'}{stale && ' · 数据过期，请勿作为当前值'}
    </p>
    <div className="grid gap-2 sm:grid-cols-3">
      <Metric label="整机 CPU" metric={data.cpu.usage} stale={stale} />
      <Metric label="逻辑 CPU 数" metric={data.cpu.logical_count} stale={stale} />
      <Metric label="物理核心数" metric={data.cpu.physical_count} stale={stale} />
      <Metric label="物理 RAM 总量" metric={data.memory.total} stale={stale} />
      <Metric label="物理 RAM 已用" metric={data.memory.used} stale={stale} />
      <Metric label="物理 RAM 可用" metric={data.memory.available} stale={stale} />
    </div>
    <p className="text-xs text-muted-foreground">GPU 来源 {data.gpu.source} · {data.gpu.scope} · 采样 {timestamp(data.gpu.sampled_at)} · 最近尝试 {timestamp(data.gpu.attempted_at)} · 查询窗口 {data.gpu.sample_window_ms} ms{data.gpu.stale && ' · GPU 数据过期 / 不可用'}</p>
    {data.gpu.reason && <p className="text-sm">GPU: {data.gpu.reason}</p>}
    {data.gpu.devices.map(device => <div key={device.index} className="grid gap-2 sm:grid-cols-2">
      <Metric label={`GPU ${device.index} 整卡利用率`} metric={device.utilization} stale={stale} />
      <Metric label={`GPU ${device.index} 显存总量`} metric={device.memory_total} stale={stale} />
      <Metric label={`GPU ${device.index} 显存已用`} metric={device.memory_used} stale={stale} />
      <Metric label={`GPU ${device.index} 显存空闲`} metric={device.memory_free} stale={stale} />
    </div>)}
    <p className="text-sm text-muted-foreground">整卡 GPU / VRAM 包含未受管理的外部程序占用，不是精确模型用量。游戏状态未检测；暂停策略不是硬件计数器。</p>
    <details>
      <summary className="cursor-pointer text-sm">管理器持有的本地进程观测 ({data.processes.length})</summary>
      <p className="text-xs text-muted-foreground">{data.process_coverage}。CPU 按整机逻辑容量归一化；工作集相加可能重复计算共享页；private/commit 不等于物理 RAM。</p>
      {data.processes.map(p => <div key={`${p.instance}/${p.generation}`} className="mt-3 space-y-2">
        <h4 className="text-sm font-medium">{p.instance} · generation {p.generation}</h4>
        <p className="text-xs text-muted-foreground">{p.coverage} · 已观测 {p.observed_processes} 个进程 · {p.reason}</p>
        <div className="grid gap-2 sm:grid-cols-3">
          <Metric label="进程树 CPU" metric={p.cpu} stale={stale} />
          <Metric label="工作集 / RSS" metric={p.working_set} stale={stale} />
          <Metric label="Private / Commit" metric={p.private_bytes} stale={stale} />
          <Metric label="进程 GPU" metric={p.gpu_utilization} stale={stale} />
          <Metric label="进程显存" metric={p.gpu_memory} stale={stale} />
        </div>
      </div>)}
    </details>
  </div>
}

export default function OwnerResources() {
  const [pending, setPending] = useState(false)
  const busy = useRef(false)
  const mounted = useRef(false)
  const writeController = useRef<AbortController | null>(null)
  const writeTimeout = useRef<number | undefined>(undefined)
  const [actionError, setActionError] = useState<string | null>(null)
  const [notice, setNotice] = useState<string | null>(null)
  const [confirmation, setConfirmation] = useState<ResourcePolicy | null>(null)
  const policy = useVisibleRead(resourcePolicyApi.get, 2000, pending)
  const metrics = useVisibleRead(resourcesApi.get, 2000)

  useEffect(() => {
    mounted.current = true
    return () => { mounted.current = false; writeController.current?.abort(); window.clearTimeout(writeTimeout.current) }
  }, [])

  const act = async (action: 'pause' | 'resume' | 'immediate') => {
    if (busy.current) return
    const before = policy.current()
    if (!before) { setActionError('策略未确认或已过期；请先刷新核对，未发送写入。'); return }
    if (action === 'immediate' && (!confirmation || before.allow_ai || confirmation.boot_id !== before.boot_id || confirmation.revision !== before.revision)) {
      setConfirmation(null); setActionError('确认期间策略已改变；请重新核对并确认，未发送释放请求。'); return
    }
    busy.current = true
    setPending(true); setNotice(null); setActionError(null); setConfirmation(null)
    policy.invalidate()
    const controller = new AbortController()
    writeController.current = controller
    const timeout = window.setTimeout(() => controller.abort(), 10000)
    writeTimeout.current = timeout
    let response: ResourcePolicy | null = null
    let failed = false
    try {
      response = action === 'immediate'
        ? await resourcePolicyApi.release(before.boot_id, before.revision, releaseRequestID(), 'immediate', controller.signal)
        : await resourcePolicyApi.update(action === 'resume', before.boot_id, before.revision, controller.signal)
    } catch (err) {
      failed = true
      if (mounted.current) setActionError(`写入失败或结果未知：${err instanceof Error ? err.message : '网络错误'}。未自动重试；以下回读不代表该写入成功。`)
    } finally {
      window.clearTimeout(timeout)
    }
    // Even a 200/202 is not evidence of current policy or completed release.
    if (mounted.current) {
      const actual = await policy.refresh()
      if (!mounted.current) { busy.current = false; return }
      if (!failed) {
        if (!actual) setActionError('写入响应已收到，但回读失败或页面隐藏；结果未确认，禁止继续写入。')
        else if (!response || actual.boot_id !== response.boot_id || actual.revision !== response.revision || actual.allow_ai !== response.allow_ai ||
          (action === 'immediate' && actual.release_operation?.id !== response.release_operation?.id)) {
          setActionError('回读发现策略或操作已改变；请核对当前状态，未自动重试。')
        } else setNotice('已回读当前状态；释放进度以独立状态为准，不承诺显存立即归零。')
      }
    }
    busy.current = false
    if (mounted.current) setPending(false)
  }

  const p = policy.data
  const disabled = pending || policy.stale || policy.reading
  return <section aria-label="AI 主人控制与资源" className="mb-8 rounded-lg border-2 border-primary/40 bg-card p-4 space-y-4">
    <div className="flex flex-wrap items-center justify-between gap-3">
      <h2 className="text-lg font-semibold">AI 资源让位</h2>
      <div className="flex flex-wrap gap-2">
        <Button size="lg" variant={p?.allow_ai === false ? 'default' : 'destructive'} disabled={disabled} onClick={() => void act(p?.allow_ai === false ? 'resume' : 'pause')}>
          {p?.allow_ai === false ? '恢复 AI' : '暂停 AI'}
        </Button>
        <Button variant="outline" disabled={disabled || !p || p.allow_ai} onClick={() => setConfirmation(policy.current())}>立即释放…</Button>
        <Button variant="ghost" disabled={pending || policy.reading || document.visibilityState !== 'visible'} onClick={() => { void policy.refresh(); void metrics.refresh() }}>刷新核对</Button>
      </div>
    </div>
    <p className="text-sm text-muted-foreground">暂停会先阻止新请求，并默认温和释放。恢复仅开放准入，不会自动加载或重放已取消请求。仅 owner 会话或显式配置的管理密钥可操作。</p>
    <div aria-live="polite" className="space-y-1 text-sm">
      <p>策略准入：<strong>{p ? p.admission === 'blocked' ? '已阻止 (blocked)' : '允许 (allowed)' : '未知'}</strong>{policy.stale && ' · 策略过期 / 未经确认，写入已禁用'}{pending && ' · 正在处理并回读…'}</p>
      <p>实际释放：{p ? releaseCopy[p.release] : '未知'}{p?.release_operation?.error_code && ` · ${p.release_operation.error_code}`}</p>
      <p>持久化：{p ? p.durable ? '已落盘 (durable)' : '未落盘 / 不持久，重启后须核对' : '未知'} · 最近回读 {policy.readAt ? new Date(policy.readAt).toLocaleString() : '无'}</p>
      {p?.release_operation && <p>释放操作：{p.release_operation.mode} · 开始 {timestamp(p.release_operation.started_at)}{p.release_operation.ended_at && ` · 结束 ${timestamp(p.release_operation.ended_at)}`}</p>}
      {policy.error && <p role="alert" className="text-destructive">策略读取失败：{policy.error}</p>}
      {actionError && <p role="alert" className="text-destructive">{actionError}</p>}
      {notice && <p>{notice}</p>}
    </div>
    <details open>
      <summary className="cursor-pointer font-medium">资源观测（只读，与释放事实分开）</summary>
      <div className="mt-3 space-y-3">
        {metrics.error && <p role="alert" className="text-amber-600">资源读取失败 / 旧数据：{metrics.error}</p>}
        {metrics.data ? <ResourceValues data={metrics.data} stale={metrics.stale} /> : <p className="text-sm text-muted-foreground">暂无资源采样，不能视为零占用。</p>}
      </div>
    </details>
    <Dialog open={confirmation !== null} onOpenChange={open => { if (!open && !pending) setConfirmation(null) }}>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>确认立即释放 AI 资源？</DialogTitle>
          <DialogDescription>这会中断正在进行的推理和加载，并请求终止管理器持有的本地进程树。不会终止未受管理的程序；请求被接受不代表进程或显存已经释放。</DialogDescription>
        </DialogHeader>
        <DialogFooter>
          <Button variant="outline" disabled={pending} onClick={() => setConfirmation(null)}>取消</Button>
          <Button variant="destructive" disabled={disabled} onClick={() => void act('immediate')}>确认立即释放</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  </section>
}
