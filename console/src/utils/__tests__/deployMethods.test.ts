import { describe, expect, it } from 'vitest'
import type { BuildStatus } from '@/api/apps'
import { gitCardState, imageCardState } from '../deployMethods'

const build = (over: Partial<BuildStatus> = {}): BuildStatus => ({
  git_configured: false,
  phase: 'none',
  ...over,
})

describe('imageCardState', () => {
  it('is active for any non-git app — image is the default mechanism', () => {
    // The earlier history-based derivation made fresh `kip deploy --image`
    // apps look unconfigured because UpdateImage skips the history write.
    // The new contract: !git_configured → always active.
    expect(imageCardState(build())).toBe('active')
  })

  it('is inactive when git is configured — image is built automatically', () => {
    expect(imageCardState(build({ git_configured: true }))).toBe('inactive')
  })

  it('treats a null buildStatus as not-git, so image card is active', () => {
    expect(imageCardState(null)).toBe('active')
  })
})

describe('gitCardState', () => {
  it('is inactive when git is not configured', () => {
    expect(gitCardState(build())).toBe('inactive')
    expect(gitCardState(null)).toBe('inactive')
  })

  it('is active when git is configured and the latest build succeeded', () => {
    expect(gitCardState(build({ git_configured: true, phase: 'Succeeded' }))).toBe('active')
  })

  it('stays active during in-flight builds', () => {
    expect(gitCardState(build({ git_configured: true, phase: 'Building' }))).toBe('active')
    expect(gitCardState(build({ git_configured: true, phase: 'Pending' }))).toBe('active')
  })

  it('is active when git is configured but no build has run yet', () => {
    expect(gitCardState(build({ git_configured: true, phase: 'none' }))).toBe('active')
  })

  it('is error when the latest build failed', () => {
    expect(gitCardState(build({ git_configured: true, phase: 'Failed' }))).toBe('error')
  })
})

describe('image vs git mutual exclusivity', () => {
  it('never shows both image and git as active for the same app', () => {
    const cases: BuildStatus[] = [
      build({ git_configured: true, phase: 'Succeeded' }),
      build({ git_configured: false }),
      build({ git_configured: true, phase: 'Failed' }),
    ]
    for (const c of cases) {
      const image = imageCardState(c)
      const git = gitCardState(c)
      // Only one may be active at a time. git=true forces image inactive;
      // git=false forces image active and git inactive (or error).
      expect(image === 'active' && git === 'active').toBe(false)
    }
  })
})
