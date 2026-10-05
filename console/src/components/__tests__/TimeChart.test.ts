// @vitest-environment happy-dom
import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import TimeChart from '../activity/TimeChart.vue'

const timestamps = [0, 30, 60, 90]

function last(events: unknown[][] | undefined): unknown[] | undefined {
  return events?.[events.length - 1]
}

describe('TimeChart', () => {
  it('stacks bars, draws lines with gaps and shows the axis maximum', () => {
    const wrapper = mount(TimeChart, {
      props: {
        label: 'requests',
        timestamps,
        bars: [{ values: [1, 2, null, 0], color: 'green' }, { values: [1, 0, null, 1], color: 'red' }],
        lines: [{ values: [3, null, 4, 5], color: 'black' }],
        floor: 1,
      },
    })
    expect(wrapper.findAll('[data-testid="chart-bar"]')).toHaveLength(4)
    const d = wrapper.get('[data-testid="chart-line"]').attributes('d')!
    expect(d.match(/M/g)).toHaveLength(2)
    expect(wrapper.text()).toContain('5')
  })

  it('draws each marker in its style with its label', () => {
    const wrapper = mount(TimeChart, {
      props: { label: 'pods', timestamps, markers: [{ time: 30, style: 'solid' }, { time: 60, style: 'dashed', label: 'bounds' }] },
    })
    expect(wrapper.find('[data-testid="chart-marker-solid"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="chart-marker-dashed"]').exists()).toBe(true)
    expect(wrapper.get('[data-testid="chart-marker-label"]').text()).toBe('bounds')
  })

  it('reports the point and marker under the pointer and clears them on leave', async () => {
    const wrapper = mount(TimeChart, { props: { label: 'cpu', timestamps, markers: [{ time: 60, style: 'solid' }] } })
    const svg = wrapper.get('svg')
    svg.element.getBoundingClientRect = () => ({ left: 0, width: 300, top: 0, height: 96, right: 300, bottom: 96, x: 0, y: 0, toJSON: () => ({}) })
    await svg.trigger('mousemove', { clientX: 200 })
    expect(last(wrapper.emitted('update:hover'))).toEqual([2])
    expect(last(wrapper.emitted('update:activeMarker'))).toEqual([0])
    await svg.trigger('mouseleave')
    expect(last(wrapper.emitted('update:hover'))).toEqual([null])
  })

  it('picks the nearest of two close markers', async () => {
    const wrapper = mount(TimeChart, { props: { label: 'cpu', timestamps: [0, 1000], markers: [{ time: 500, style: 'solid' }, { time: 510, style: 'dashed' }] } })
    const svg = wrapper.get('svg')
    svg.element.getBoundingClientRect = () => ({ left: 0, width: 1000, top: 0, height: 96, right: 1000, bottom: 96, x: 0, y: 0, toJSON: () => ({}) })
    await svg.trigger('mousemove', { clientX: 509 })
    expect(last(wrapper.emitted('update:activeMarker'))).toEqual([1])
  })

  it('draws the hover line where another chart points', () => {
    const wrapper = mount(TimeChart, { props: { label: 'cpu', timestamps, hover: 1 } })
    expect(wrapper.get('[data-testid="chart-hover"]').attributes('x1')).toBe(String(1000 / 3))
  })
})
