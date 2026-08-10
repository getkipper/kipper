import client from './client'

export interface VolumeMount {
  app: string
  path: string
}

export interface Volume {
  name: string
  size: string
  status: string
  access: string
  mounts: VolumeMount[]
}

export async function fetchVolumes(project: string): Promise<Volume[]> {
  const { data } = await client.get<Volume[]>(`/projects/${project}/volumes`)
  return data || []
}

export async function createVolume(project: string, name: string, size: string): Promise<void> {
  await client.post(`/projects/${project}/volumes`, { name, size })
}

export async function deleteVolume(project: string, name: string): Promise<void> {
  await client.delete(`/projects/${project}/volumes/${name}`)
}

export async function mountVolume(project: string, volumeName: string, appName: string, mountPath: string): Promise<void> {
  await client.post(`/projects/${project}/volumes/mount`, {
    volume_name: volumeName,
    app_name: appName,
    mount_path: mountPath,
  })
}

export async function unmountVolume(project: string, volumeName: string, appName: string): Promise<void> {
  await client.post(`/projects/${project}/volumes/unmount`, {
    volume_name: volumeName,
    app_name: appName,
  })
}
