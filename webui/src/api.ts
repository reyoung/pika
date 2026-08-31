import type { ArtifactPreview, NodeDetail, Snapshot } from './types'

export const tokenStorageKey = 'pika-go-webui-token'

export function importFragmentToken(): string {
  const fragment = new URLSearchParams(window.location.hash.slice(1))
  const token = fragment.get('token')?.trim()
  if (token) sessionStorage.setItem(tokenStorageKey, token)
  if (window.location.hash) history.replaceState(null, '', `${window.location.pathname}${window.location.search}`)
  return token || sessionStorage.getItem(tokenStorageKey) || ''
}

async function authenticatedFetch(token: string, path: string): Promise<Response> {
  return fetch(path, {
    headers: { Authorization: `Bearer ${token}` },
    cache: 'no-store',
  })
}

export async function loadSnapshot(token: string, limit: number): Promise<Snapshot> {
  const response = await authenticatedFetch(token, `/api/v1/workbench/snapshot?terminal_attempt_limit=${limit}`)
  if (!response.ok) throw new Error(response.status === 401 ? 'Token rejected' : `Snapshot request failed (${response.status})`)
  return response.json() as Promise<Snapshot>
}

export async function loadNode(token: string, kind: string, id: string): Promise<NodeDetail> {
  const response = await authenticatedFetch(token, `/api/v1/workbench/nodes/${encodeURIComponent(kind)}/${encodeURIComponent(id)}`)
  if (!response.ok) throw new Error(`Node request failed (${response.status})`)
  return response.json() as Promise<NodeDetail>
}

export async function loadArtifactPreview(token: string, id: string): Promise<ArtifactPreview | Blob> {
  const response = await authenticatedFetch(token, `/api/v1/workbench/artifacts/${encodeURIComponent(id)}`)
  if (!response.ok) throw new Error(`Artifact request failed (${response.status})`)
  if (response.headers.get('content-type')?.includes('application/json')) return response.json() as Promise<ArtifactPreview>
  return response.blob()
}

export async function downloadArtifact(token: string, id: string, filename: string): Promise<void> {
  const response = await authenticatedFetch(token, `/api/v1/workbench/artifacts/${encodeURIComponent(id)}?download=1`)
  if (!response.ok) throw new Error(`Artifact download failed (${response.status})`)
  const blob = await response.blob()
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = filename
  anchor.click()
  URL.revokeObjectURL(url)
}
