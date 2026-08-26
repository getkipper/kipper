import type { Capability, ProjectRole } from '@/api/projects'

/**
 * What each built-in role may do, for tests that need a Project fixture.
 *
 * This is a fixture, not a second copy of the model: production code never
 * derives capabilities from a role name, it is told them by the server. What
 * this encodes is the expectation a test is asserting against, and if the
 * server's catalogue and this disagree the console's own behaviour is
 * unaffected — only the test's premise would be wrong.
 *
 * It mirrors the catalogue's own built-in sets exactly. A fixture that carried
 * a subset would have tests asserting against a member who cannot do things
 * the real one can, which is how a passing test comes to mean nothing.
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
