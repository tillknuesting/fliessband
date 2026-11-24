import { useEffect, useState } from 'react'
import { getJSON, postJSON } from './api'
import type { Deployment, DeployState } from './types'

/** The deploy targets as their clusters report them, polled faster while something deploys. */
export function useDeployState(enabled = true) {
  const [state, setState] = useState<DeployState | null>(null)
  const busy = !!state?.deployments.some((d) => d.status === 'running')
  useEffect(() => {
    if (!enabled) return
    let live = true
    const tick = () => getJSON<DeployState>('/api/deploy').then((s) => live && setState(s), () => {})
    tick()
    const t = setInterval(tick, busy ? 1500 : 5000)
    return () => {
      live = false
      clearInterval(t)
    }
  }, [enabled, busy])
  return state
}

/** Follows one deployment until it finishes. */
export function useDeployment(id: string | null) {
  const [dep, setDep] = useState<Deployment | null>(null)
  useEffect(() => {
    setDep(null)
    if (!id) return
    let live = true
    let t: ReturnType<typeof setTimeout>
    const tick = () =>
      getJSON<Deployment>(`/api/deployments/${encodeURIComponent(id)}`).then(
        (d) => {
          if (!live) return
          setDep(d)
          if (d.status === 'running') t = setTimeout(tick, 700)
        },
        () => live && (t = setTimeout(tick, 2000)),
      )
    tick()
    return () => {
      live = false
      clearTimeout(t)
    }
  }, [id])
  return dep
}

export const startDeploy = (run: string, target: string) => postJSON<{ id: string }>(`/api/runs/${encodeURIComponent(run)}/deploy`, { target })

export const removeService = (target: string, service: string) =>
  postJSON<Deployment>(`/api/deploy/${encodeURIComponent(target)}/${encodeURIComponent(service)}/remove`, {})
