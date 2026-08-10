import client from './client'

export interface InlineFunction {
  name: string
  runtime: string
  code: string
  // Trigger configuration round-tripped from the Function CR so the
  // edit form can re-populate after a page refresh.
  trigger?: string
  schedule?: string
  source?: string
  query?: string
  mark_done?: string
  redis_list?: string
  bucket?: string
}

export interface InlineFunctionUpdate {
  code: string
  runtime?: string
  trigger?: string
  schedule?: string
  source?: string
  query?: string
  mark_done?: string
  redis_list?: string
  bucket?: string
}

export interface CreateInlineBinding {
  service: string
  prefix?: string
  database?: string
}

export interface CreateInlinePayload {
  name: string
  runtime: string
  code: string
  // Trigger config; "" defaults to http on the server.
  trigger?: string
  schedule?: string
  source?: string
  query?: string
  mark_done?: string
  redis_list?: string
  bucket?: string
  // Configuration the form collects in one shot.
  env?: Record<string, string>
  secrets?: Record<string, string>
  dependencies?: Record<string, string>
  bindings?: CreateInlineBinding[]
}

export interface CreateInlineResponse {
  status: string
  name: string
  runtime: string
  url: string
}

export async function createInlineFunction(project: string, payload: CreateInlinePayload): Promise<CreateInlineResponse> {
  const { data } = await client.post<CreateInlineResponse>(`/projects/${project}/inline-functions`, payload)
  return data
}

export async function getInlineFunctionCode(project: string, name: string): Promise<InlineFunction> {
  const { data } = await client.get<InlineFunction>(`/projects/${project}/inline-functions/${name}/code`)
  return data
}

// updateInlineFunctionCode round-trips code, runtime, and trigger
// config so the edit form can fix any of them in one save. Runtime
// is here because the dropdown defaults to Node and users routinely
// paste Python and forget to switch — and trigger config is here
// because cron schedules and event sources are mutable too.
export async function updateInlineFunctionCode(project: string, name: string, update: InlineFunctionUpdate): Promise<void> {
  await client.put(`/projects/${project}/inline-functions/${name}/code`, update)
}
