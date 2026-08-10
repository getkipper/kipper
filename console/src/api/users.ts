import client from './client'

export interface User {
  email: string
  role: string
}

export interface CreateUserPayload {
  email: string
  password: string
  role: string
}

export async function fetchUsers(): Promise<User[]> {
  const { data } = await client.get<User[]>('/users')
  return data
}

export async function createUser(payload: CreateUserPayload): Promise<User> {
  const { data } = await client.post<User>('/users', payload)
  return data
}

export async function updateUserRole(email: string, role: string): Promise<void> {
  await client.put(`/users/${encodeURIComponent(email)}/role`, { role })
}

export async function resetUserPassword(email: string): Promise<{ password: string }> {
  const { data } = await client.post<{ password: string }>(`/users/${encodeURIComponent(email)}/reset-password`)
  return data
}

export async function deleteUser(email: string): Promise<void> {
  await client.delete(`/users/${encodeURIComponent(email)}`)
}

export async function fetchMe(): Promise<{ email: string; role: string }> {
  const { data } = await client.get<{ email: string; role: string }>('/me')
  return data
}
