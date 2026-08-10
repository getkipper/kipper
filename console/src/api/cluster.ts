import client from './client'
import type { ClusterStatus, NodeInfo } from './types'

export async function fetchClusterStatus(): Promise<ClusterStatus> {
  const { data } = await client.get<ClusterStatus>('/cluster/status')
  return data
}

export async function fetchNodes(): Promise<NodeInfo[]> {
  const { data } = await client.get<NodeInfo[]>('/nodes')
  return data
}
