import { describe, expect, it } from 'vitest'
import { areaPaths, indexAtFraction, linePath, niceMax, stackedBars, stepPath, timeScale } from '../activityChart'

const x = (i: number) => i * 10
const y = (v: number) => 100 - v

describe('linePath', () => {
  it('breaks the line at a null', () => {
    expect(linePath([1, 2, null, 4, 5], x, y)).toBe('M0,99 L10,98 M30,96 L40,95')
  })

  it('draws a lone point between gaps as a dot', () => {
    expect(linePath([null, 3, null], x, y)).toBe('M10,97 l0,0')
  })

  it('is empty when every value is null', () => {
    expect(linePath([null, null], x, y)).toBe('')
  })
})

describe('stepPath', () => {
  it('holds each value until the next point and breaks at a null', () => {
    expect(stepPath([1, 2, null, 3, 4], x, y)).toBe('M0,99 H10 V98 H20 M30,97 H40 V96')
  })
})

describe('areaPaths', () => {
  it('fills between two series, one shape per unbroken run', () => {
    expect(areaPaths([1, 1, null, 2, 2], [3, 3, 5, 4, 4], x, y)).toEqual([
      'M0,97 L10,97 L10,99 L0,99 Z',
      'M30,96 L40,96 L40,98 L30,98 Z',
    ])
  })
})

describe('stackedBars', () => {
  it('stacks the classes at each point and skips zero and null', () => {
    const bars = stackedBars([[2, null], [3, 1]], x, y, 8)
    expect(bars).toEqual([
      { layer: 0, index: 0, x: -4, y: 98, width: 8, height: 2 },
      { layer: 1, index: 0, x: -4, y: 95, width: 8, height: 3 },
      { layer: 1, index: 1, x: 6, y: 99, width: 8, height: 1 },
    ])
  })
})

describe('niceMax', () => {
  it('rounds the largest value up to a round number', () => {
    expect(niceMax([null, 7.2, 3])).toBe(8)
    expect(niceMax([130])).toBe(150)
    expect(niceMax([0.35])).toBe(0.4)
  })

  it('keeps a floor so a quiet chart is not drawn at full height', () => {
    expect(niceMax([0, null], 5)).toBe(5)
    expect(niceMax([], 1)).toBe(1)
  })
})

describe('timeScale', () => {
  it('maps the first and last timestamp onto the edges and anything between linearly', () => {
    const sx = timeScale([100, 200, 300], 600)
    expect(sx(100)).toBe(0)
    expect(sx(300)).toBe(600)
    expect(sx(150)).toBe(150)
  })
})

describe('indexAtFraction', () => {
  it('picks the nearest point', () => {
    expect(indexAtFraction(0, 5)).toBe(0)
    expect(indexAtFraction(0.6, 5)).toBe(2)
    expect(indexAtFraction(1.2, 5)).toBe(4)
    expect(indexAtFraction(0.5, 0)).toBeNull()
  })
})
