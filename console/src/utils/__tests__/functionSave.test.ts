import { describe, expect, it } from 'vitest'
import { sameMap, saveInOrder, saveSummary } from '../functionSave'

describe('sameMap', () => {
  it('compares keys and values, not order', () => {
    expect(sameMap({ a: '1', b: '2' }, { b: '2', a: '1' })).toBe(true)
    expect(sameMap({ a: '1' }, { a: '2' })).toBe(false)
    expect(sameMap({ a: '1' }, {})).toBe(false)
  })
})

describe('saveInOrder', () => {
  it('runs each part after the previous one finished, skipping unchanged parts', async () => {
    const order: string[] = []
    const step = (label: string) => ({ label, run: async () => { order.push(label) } })
    const result = await saveInOrder([step('code'), { ...step('env'), skip: true }, step('dependencies')])

    expect(order).toEqual(['code', 'dependencies'])
    expect(result).toEqual({ saved: ['code', 'dependencies'], failed: undefined, notTried: [] })
  })

  it('refuses a blocked part without running it', async () => {
    let ran = false
    const result = await saveInOrder([
      { label: 'code', run: async () => {} },
      { label: 'environment variables', blocked: 'they could not be read when the page loaded; reload before changing them', run: async () => { ran = true } },
    ])

    expect(ran).toBe(false)
    expect(result.saved).toEqual(['code'])
    expect(result.failed).toEqual({ label: 'environment variables', error: 'they could not be read when the page loaded; reload before changing them' })
  })

  it('stops at the first failure and reports what was and was not saved', async () => {
    const result = await saveInOrder([
      { label: 'code', run: async () => {} },
      { label: 'environment variables', run: async () => { throw new Error('conflict') } },
      { label: 'dependencies', run: async () => {} },
    ])

    expect(result.saved).toEqual(['code'])
    expect(result.failed?.label).toBe('environment variables')
    expect(result.notTried).toEqual(['dependencies'])
  })
})

describe('saveSummary', () => {
  it('names what failed and what was saved before it', () => {
    expect(saveSummary({ saved: ['code'], failed: { label: 'dependencies', error: 'conflict' }, notTried: ['secrets'] }))
      .toBe('Saved code. Could not save dependencies: conflict. Not saved: secrets.')
    expect(saveSummary({ saved: [], failed: { label: 'code', error: 'forbidden' }, notTried: [] }))
      .toBe('Could not save code: forbidden.')
  })
})
