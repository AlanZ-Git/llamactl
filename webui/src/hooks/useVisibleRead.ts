import { useCallback, useEffect, useRef, useState } from 'react'

const READ_TIMEOUT_MS = 8000
export const POLICY_FRESH_MS = 10000

// One visible-only polling loop per panel, with bounded requests and last-read
// invalidation. Resource data never grants owner write authority.
export function useVisibleRead<T>(fetcher: (signal?: AbortSignal) => Promise<T>, interval: number, paused = false) {
  const [data, setData] = useState<T | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [trusted, setTrusted] = useState(false)
  const [reading, setReading] = useState(false)
  const [readAt, setReadAt] = useState(0)
  const [clock, setClock] = useState(Date.now())
  const mounted = useRef(false)
  const blocked = useRef(paused)
  blocked.current = paused
  const request = useRef<AbortController | null>(null)
  const requestTimeout = useRef<number | undefined>(undefined)
  const serial = useRef(0)
  const state = useRef<{ data: T | null; trusted: boolean; at: number; reading: boolean }>({ data: null, trusted: false, at: 0, reading: false })
  const denied = useRef(false)

  const invalidate = useCallback(() => {
    serial.current++
    request.current?.abort()
    window.clearTimeout(requestTimeout.current)
    requestTimeout.current = undefined
    state.current.trusted = false
    state.current.reading = false
    if (mounted.current) { setTrusted(false); setReading(false) }
  }, [])

  const refresh = useCallback(async (): Promise<T | null> => {
    if (!mounted.current || document.visibilityState !== 'visible') return null
    invalidate()
    const id = serial.current
    const controller = new AbortController()
    request.current = controller
    const timeout = window.setTimeout(() => controller.abort(), READ_TIMEOUT_MS)
    requestTimeout.current = timeout
    const started = Date.now()
    state.current.reading = true
    setReading(true)
    try {
      const result = await fetcher(controller.signal)
      if (!mounted.current || serial.current !== id || controller.signal.aborted || document.visibilityState !== 'visible') return null
      const elapsed = Date.now() - started
      const fresh = elapsed >= 0 && elapsed < POLICY_FRESH_MS
      state.current = { data: result, trusted: fresh, at: started, reading: false }
      denied.current = false
      setData(result); setTrusted(fresh); setReadAt(started); setError(null); setClock(Date.now())
      return result
    } catch (err) {
      if (mounted.current && serial.current === id) {
        const message = controller.signal.aborted ? '读取超时或已取消；状态未经确认' : err instanceof Error ? err.message : '读取失败'
        denied.current = message === 'Authentication required'
        state.current.trusted = false
        setTrusted(false); setError(message)
      }
      return null
    } finally {
      window.clearTimeout(timeout)
      if (requestTimeout.current === timeout) requestTimeout.current = undefined
      if (mounted.current && serial.current === id) { state.current.reading = false; setReading(false) }
    }
  }, [fetcher, invalidate])

  useEffect(() => {
    mounted.current = true
    let timer: number | undefined
    const visibility = () => {
      window.clearInterval(timer)
      invalidate()
      setClock(Date.now())
      if (document.visibilityState === 'visible') {
        if (!blocked.current && !denied.current) void refresh()
        timer = window.setInterval(() => {
          setClock(Date.now())
          if (!blocked.current && !denied.current && !state.current.reading) void refresh()
        }, interval)
      }
    }
    visibility()
    document.addEventListener('visibilitychange', visibility)
    return () => {
      mounted.current = false
      window.clearInterval(timer)
      document.removeEventListener('visibilitychange', visibility)
      invalidate()
    }
  }, [interval, invalidate, refresh])

  const current = useCallback(() => {
    const s = state.current
    const age = Date.now() - s.at
    return mounted.current && document.visibilityState === 'visible' && s.trusted && !s.reading && age >= 0 && age < POLICY_FRESH_MS ? s.data : null
  }, [])

  return { data, error, reading, readAt, stale: !trusted || clock < readAt || clock - readAt >= POLICY_FRESH_MS, refresh, invalidate, current }
}
