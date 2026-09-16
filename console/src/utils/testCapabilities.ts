import type { Capability, ProjectRole } from '@/api/projects'

/**
 * Built-in capability sets for Project test fixtures. Keep these aligned with
 * controller/pkg/capability; production UI uses the server-supplied lists.
 */
const byRole: Record<string, Capability[]> = {
  viewer: [
    'database.read', 'env.read', 'files.read', 'kipper.read', 'members.read', 'pods.logs.read',
    'project.read', 'storage.read', 'webhook.reveal', 'workloads.read',
  ],
  deployer: [
    'apikeys.manage', 'database.read', 'database.write', 'env.read', 'env.reveal', 'env.write',
    'files.read', 'files.write', 'kipper.read', 'kipper.write', 'members.read',
    'pods.logs.read', 'project.read', 'storage.read', 'storage.write', 'terminal.open',
    'webhook.reveal', 'workloads.read', 'workloads.restart',
  ],
  owner: [
    'apikeys.manage', 'database.read', 'database.write', 'env.read', 'env.reveal', 'env.write',
    'files.read', 'files.write', 'kipper.read', 'kipper.write', 'members.manage',
    'members.read', 'pods.exec', 'pods.logs.read', 'project.delete', 'project.read',
    'project.settings', 'secrets.read', 'secrets.write', 'storage.read', 'storage.write',
    'terminal.open', 'webhook.reveal', 'workloads.read', 'workloads.restart',
  ],
}


/** The capabilities a member of this role holds, or none for an unknown role. */
export function capabilitiesForRole(role: ProjectRole): Capability[] {
  return byRole[role] ?? []
}
